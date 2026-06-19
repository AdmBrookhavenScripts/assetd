import discord
from discord import app_commands
import aiohttp
import asyncio
import re
import os
import zipfile
import uuid
import logging
from urllib.parse import urljoin, urlparse, urlunparse
from colorama import init, Fore, Style

init(autoreset=True)

class ColoredFormatter(logging.Formatter):
    COLORS = {
        logging.DEBUG: Fore.CYAN,
        logging.INFO: Fore.GREEN,
        logging.WARNING: Fore.YELLOW,
        logging.ERROR: Fore.RED,
        logging.CRITICAL: Fore.RED + Style.BRIGHT,
    }

    def format(self, record):
        log_color = self.COLORS.get(record.levelno, Fore.WHITE)
        record.msg = f"{log_color}{record.msg}{Style.RESET_ALL}"
        return super().format(record)

logger = logging.getLogger('RobloxAssetBot')
logger.setLevel(logging.DEBUG)
ch = logging.StreamHandler()
ch.setFormatter(ColoredFormatter('%(asctime)s - %(levelname)s - %(message)s', '%H:%M:%S'))
logger.addHandler(ch)

DISCORD_TOKEN = os.getenv("DISCORD_TOKEN")
ROBLOX_COOKIE = os.getenv("ROBLOX_COOKIE")

def load_fallback_games():
    place_ids = []

    if not os.path.exists("fallback-games.txt"):
        return place_ids

    with open("fallback-games.txt", "r", encoding="utf-8") as f:
        for line in f:
            line = line.strip()

            if not line or line.startswith("#"):
                continue

            place_id = line.split("#", 1)[0].strip()

            if place_id.isdigit():
                place_ids.append(int(place_id))

    return place_ids

FALLBACK_GAMES = load_fallback_games()

NO_BINARY_TYPES = [21, 34]

async def upload_litterbox(file_path: str, expire="72h"):
    url = "https://litterbox.catbox.moe/resources/internals/api.php"
    try:
        async with aiohttp.ClientSession() as session:
            with open(file_path, 'rb') as f:
                data = aiohttp.FormData()
                data.add_field('reqtype', 'fileupload')
                data.add_field('time', expire)
                data.add_field('fileToUpload', f, filename=os.path.basename(file_path))
                
                async with session.post(url, data=data) as response:
                    if response.status == 200:
                        return await response.text()
                    else:
                        return f"Erro: HTTP {response.status}"
    except Exception as e:
        return f"Erro: {str(e)}"

def detect_file_extension(content: bytes, content_type: str, fallback_ext: str) -> str:
    if content.startswith(b'#EXTM3U'):
        return '.m3u8'
    if content.startswith(b'\x89PNG\r\n\x1a\n'):
        return '.png'
    if content.startswith(b'OggS'):
        return '.ogg'
    if content.startswith(b'\x1aE\xdf\xa3'):
        return '.webm'
    if content.startswith(b'<roblox!'):
        return '.rbxm'
    if content.startswith(b'<roblox'):
        return '.rbxmx'
    if content.startswith(b'version '):
        return '.mesh'
    if content.startswith(b'{"') or content.startswith(b'['):
        return '.json'
    
    ctype = content_type.lower()
    if 'image/png' in ctype: return '.png'
    if 'audio/ogg' in ctype: return '.ogg'
    if 'video/webm' in ctype: return '.webm'
    if 'application/xml' in ctype: return '.rbxmx'
    if 'application/json' in ctype: return '.json'
    if 'text/plain' in ctype: return '.txt'
    
    return fallback_ext

async def fetch_creator_games(session: aiohttp.ClientSession, creator_id: int, creator_type: str):
    games_info = []
    url = f"https://games.roproxy.com/v2/groups/{creator_id}/games?accessFilter=2&sortOrder=Asc&limit=50" if creator_type == "Group" else f"https://games.roproxy.com/v2/users/{creator_id}/games?accessFilter=2&sortOrder=Asc&limit=50"
    
    try:
        async with session.get(url) as response:
            if response.status == 200:
                data = await response.json()
                for game in data.get("data", []):
                    pid = game["rootPlace"]["id"] if "rootPlace" in game and "id" in game["rootPlace"] else None
                    uid = game.get("id")
                    if pid or uid:
                        games_info.append({"place_id": pid, "universe_id": uid})
    except Exception as e:
        logger.warning(f"Falha ao buscar experiencias do criador {creator_id}: {e}")
    return games_info

async def fetch_asset_details(session: aiohttp.ClientSession, asset_id: str, max_retries=10):
    url = f"https://economy.roproxy.com/v2/assets/{asset_id}/details"
    for attempt in range(max_retries):
        try:
            async with session.get(url) as response:
                if response.status == 200:
                    return await response.json()
                elif response.status in [400, 403]:
                    return await response.json()
                elif response.status == 429:
                    await asyncio.sleep(0.5 * (attempt + 1))
                    continue
                else:
                    break
        except Exception:
            await asyncio.sleep(0.5)
    return None

async def fetch_asset_location(session: aiohttp.ClientSession, asset_id: str, place_id=None, cookie=None, universe_id=None):
    url = 'https://assetdelivery.roproxy.com/v2/assets/batch'
    body_array = [{
        "assetId": asset_id,
        "requestId": "0"
    }]
    
    headers = {
        "User-Agent": "Roblox/WinInet",
        "Content-Type": "application/json",
        "Accept": "*/*",
        "Roblox-Browser-Asset-Request": "false"
    }
    
    if cookie:
        headers["Cookie"] = f".ROBLOSECURITY={cookie}"
    if place_id:
        headers["Roblox-Place-Id"] = str(place_id)
    if universe_id:
        headers["Roblox-Universe-Id"] = str(universe_id)

    try:
        async with session.post(url, headers=headers, json=body_array) as response:
            if response.status == 200:
                locations = await response.json()
                if locations and len(locations) > 0:
                    obj = locations[0]
                    if obj.get("locations") and obj["locations"][0].get("location"):
                        return obj["locations"][0]["location"]
    except Exception as e:
        logger.debug(f"Erro ao buscar localizacao do asset {asset_id} (Place: {place_id}, Universe: {universe_id}): {e}")
    return None

def sanitize_filename(name: str) -> str:
    sanitized = re.sub(r'[\\/*?"<>|]', '', name)
    return sanitized.replace(" ", "_")

async def convert_media(input_path: str, format: str, quality: str) -> str:
    if not format or (input_path.endswith(format) and quality == 'original'):
        return input_path

    input_dir = os.path.dirname(input_path) or '.'
    input_name = os.path.basename(input_path)
    temp_output_name = input_name.rsplit('.', 1)[0] + "_mod" + format
    temp_output_path = os.path.join(input_dir, temp_output_name)

    cmd = ['ffmpeg', '-y', '-i', input_name]

    is_audio = format in ['.mp3', '.wav', '.ogg']
    if is_audio:
        if format == '.mp3':
            cmd.extend(['-c:a', 'libmp3lame'])
        elif format == '.wav':
            cmd.extend(['-c:a', 'pcm_s16le'])
        elif format == '.ogg':
            cmd.extend(['-c:a', 'libvorbis'])

        if quality == 'high':
            cmd.extend(['-b:a', '320k'])
        elif quality == 'medium':
            cmd.extend(['-b:a', '192k'])
        elif quality == 'low':
            cmd.extend(['-b:a', '128k'])
        elif quality == 'original' and format == '.mp3':
            cmd.extend(['-q:a', '2'])
    else:
        if format in ['.mp4', '.mov', '.webm']:
            if quality == '1080p':
                cmd.extend(['-vf', 'scale=-2:1080'])
            elif quality == '720p':
                cmd.extend(['-vf', 'scale=-2:720'])
            elif quality == '480p':
                cmd.extend(['-vf', 'scale=-2:480'])

    cmd.append(temp_output_name)

    try:
        process = await asyncio.create_subprocess_exec(
            *cmd,
            stdout=asyncio.subprocess.PIPE,
            stderr=asyncio.subprocess.PIPE,
            cwd=os.path.abspath(input_dir)
        )

        stdout, stderr = await process.communicate()

        if stdout:
            logger.info(stdout.decode(errors="ignore"))

        if stderr:
            logger.error(stderr.decode(errors="ignore"))

        logger.info(f"FFmpeg return code: {process.returncode}")

        if process.returncode != 0:
            return input_path

        if os.path.exists(temp_output_path) and os.path.getsize(temp_output_path) > 0:
            try:
                os.remove(input_path)
                final_output_path = os.path.join(input_dir, input_name.rsplit('.', 1)[0] + format)
                os.rename(temp_output_path, final_output_path)
                return final_output_path
            except Exception:
                return temp_output_path

    except Exception as e:
        logger.error(f"Erro no FFmpeg: {e}")

    return input_path

async def process_hls_playlist(session: aiohttp.ClientSession, m3u8_path: str, base_url: str) -> str:
    logger.info(f"Processando playlist HLS: {m3u8_path}")
    try:
        with open(m3u8_path, 'r', encoding='utf-8') as f:
            m3u8_content = f.read()

        lines = m3u8_content.splitlines()
        logger.info(f"Tipo de playlist detectada. Primeiras linhas: {lines[:5]}")

        rbx_base_uri = None
        for line in lines:
            match = re.search(r'#EXT-X-DEFINE:NAME="RBX-BASE-URI",VALUE="([^"]+)"', line)
            if match:
                rbx_base_uri = match.group(1)
                if not rbx_base_uri.endswith('/'):
                    rbx_base_uri += '/'
                logger.info(f"RBX-BASE-URI detectado: {rbx_base_uri}")
                break

        best_playlist_url = None
        streams = []
        
        for i, line in enumerate(lines):
            if line.startswith('#EXT-X-STREAM-INF'):
                if i + 1 < len(lines):
                    streams.append((line, lines[i+1]))
        
        logger.info(f"Quantidade de streams encontrados: {len(streams)}")
        
        if streams:
            best_stream = None
            max_height = -1

            for info, url in streams:
                res_match = re.search(r'RESOLUTION=\d+x(\d+)', info)
                if res_match:
                    height = int(res_match.group(1))
                    if height > max_height:
                        max_height = height
                        best_stream = (info, url)

            if best_stream:
                best_playlist_url = best_stream[1]
                logger.info(f"Stream selecionado (Maior Resolução): {best_stream[0]}")
            else:
                best_playlist_url = streams[0][1]
                for info, url in streams:
                    if '720' in info or '720' in url:
                        best_playlist_url = url
                        best_stream = (info, url)
                        break
                if not best_stream:
                    best_stream = streams[0]
                logger.info(f"Stream selecionado (Fallback): {best_stream[0]}")

        def get_url_with_auth(base_path, target_path, master_url):
            joined = urljoin(base_path, target_path)
            parsed_joined = urlparse(joined)
            parsed_master = urlparse(master_url)
            
            if not urlparse(target_path).query:
                if parsed_joined.netloc == parsed_master.netloc:
                    joined = urlunparse(parsed_joined._replace(query=parsed_master.query))
                
            return joined

        headers = {
            "User-Agent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/122.0.0.0 Safari/537.36"
        }

        if not best_playlist_url:
            best_playlist_url = base_url
            internal_m3u8_content = m3u8_content
        else:
            if "{$RBX-BASE-URI}" in best_playlist_url and rbx_base_uri:
                best_playlist_url = best_playlist_url.replace(
                    "{$RBX-BASE-URI}",
                    rbx_base_uri.rstrip("/")
                )
            else:
                best_playlist_url = get_url_with_auth(
                    base_url,
                    best_playlist_url,
                    base_url
                )

            logger.info(f"URL INTERNA = {best_playlist_url}")

            async with session.get(best_playlist_url, headers=headers) as resp:
                if resp.status != 200:
                    logger.error(f"Falha ao baixar playlist interna: {resp.status}")
                    return None
                internal_m3u8_content = await resp.text()

        segments = [line for line in internal_m3u8_content.splitlines() if line and not line.startswith('#')]
        
        if not segments:
            logger.error("Nenhum segmento encontrado na playlist HLS.")
            return None

        output_dir = os.path.dirname(m3u8_path) or '.'
        base_name = os.path.basename(m3u8_path).rsplit('.', 1)[0]
        
        segment_files = []
        logger.info(f"Quantidade de segmentos encontrados: {len(segments)}")
        logger.info(f"Baixando {len(segments)} segmentos HLS para {base_name}...")
        
        segments_base_path = best_playlist_url

        for i, seg in enumerate(segments):
            seg_url = get_url_with_auth(segments_base_path, seg, base_url)
            
            clean_url = seg_url.split('?')[0]
            filename = clean_url.split('/')[-1]
            if '.' in filename:
                ext = '.' + filename.split('.')[-1]
            else:
                ext = '.webm'
            
            seg_path = os.path.join(output_dir, f"{base_name}_seg_{i:04d}{ext}")
            
            async with session.get(seg_url, headers=headers) as resp:
                if resp.status == 200:
                    content = await resp.read()
                    with open(seg_path, 'wb') as f:
                        f.write(content)
                    segment_files.append(seg_path)
                    logger.info(f"Segmento {i:04d} baixado | Extensão: {ext} | Tamanho: {len(content)} bytes")
                else:
                    logger.error(f"Falha ao baixar segmento HLS {clean_url} (HTTP {resp.status})")

        if not segment_files:
            return None

        list_name = f"{base_name}_list.txt"
        list_path = os.path.join(output_dir, list_name)
        with open(list_path, 'w', encoding='utf-8') as f:
            for sf in segment_files:
                f.write(f"file '{os.path.basename(sf)}'\n")

        webm_name = f"{base_name}.webm"
        webm_output = os.path.join(output_dir, webm_name)
        logger.info(f"Concatenando segmentos em {webm_name}...")
        
        cmd = ['ffmpeg', '-y', '-f', 'concat', '-safe', '0', '-i', list_name, '-c', 'copy', webm_name]
        
        process = await asyncio.create_subprocess_exec(
            *cmd, 
            stdout=asyncio.subprocess.PIPE, 
            stderr=asyncio.subprocess.PIPE,
            cwd=os.path.abspath(output_dir)
        )
        stdout, stderr = await process.communicate()
        
        if process.returncode != 0:
            logger.error("Falha na reconstrução HLS.")
            logger.error(f"Motivo: FFmpeg falhou com código de retorno {process.returncode}")
            return None

        logger.info(f"Resultado final da concatenação HLS: Sucesso. Salvo em {webm_output}")

        try:
            os.remove(m3u8_path)
            os.remove(list_path)
            for sf in segment_files:
                os.remove(sf)
        except Exception as e:
            logger.warning(f"Erro ao limpar arquivos temporários HLS: {e}")

        return webm_output

    except Exception as e:
        logger.error(f"Erro geral processando HLS: {e}")
        return None

async def fetch_version_fallback(session: aiohttp.ClientSession, asset_id: str, cookie: str = None, max_versions=10):
    for version in range(1, max_versions + 1):
        url = f"https://assetdelivery.roproxy.com/v1/asset/?id={asset_id}&version={version}"
        headers = {
            "User-Agent": "Roblox/WinInet",
            "Roblox-Browser-Asset-Request": "false"
        }
        
        if cookie:
            headers["Cookie"] = f".ROBLOSECURITY={cookie}"
            
        try:
            async with session.get(url, headers=headers, allow_redirects=True) as response:
                if response.status == 200:
                    content_type = response.headers.get('Content-Type', '')
                    if 'text/html' not in content_type.lower() and 'application/json' not in content_type.lower():
                        logger.info(f"Asset {asset_id} - Sucesso ao recuperar a versao {version} que escapou da moderacao!")
                        return url
        except Exception as e:
            logger.debug(f"Erro ao testar versao {version} do asset {asset_id}: {e}")
            
        await asyncio.sleep(0.5)
        
    return None

async def download_core(session: aiohttp.ClientSession, asset_id: str):
    details = await fetch_asset_details(session, asset_id)
    
    asset_name = str(asset_id)
    asset_type_id = None
    creator_id = None
    creator_type = None

    if details and "errors" not in details:
        asset_name = details.get("Name", str(asset_id))
        asset_type_id = details.get("AssetTypeId")
        creator = details.get("Creator", {})
        creator_id = creator.get("CreatorTargetId")
        creator_type = creator.get("CreatorType")
    else:
        logger.warning(f"Asset {asset_id} - Detalhes negados (provavelmente moderado). Forcando bypass direto...")

    sanitized_name = sanitize_filename(asset_name)
    logger.info(f"Processando Asset {asset_id} | Nome: {sanitized_name} | TypeID: {asset_type_id}")

    if asset_type_id in NO_BINARY_TYPES:
        msg = f"Asset {asset_id} e do tipo sem arquivo binario."
        logger.warning(msg)
        return None, msg

    asset_url = None

    if asset_type_id:
        logger.info(f"Asset {asset_id} - Tentando obter URL de forma publica...")
        asset_url = await fetch_asset_location(session, asset_id)
        
        if asset_url:
            logger.info(f"Asset {asset_id} - URL publica obtida com sucesso!")
        else:
            logger.info(f"Asset {asset_id} - Acesso publico negado. Tentando fallback com PlaceIds/UniverseIds e Cookie...")
            
            if creator_id:
                games_info = await fetch_creator_games(session, creator_id, creator_type)
                if games_info:
                    for g in games_info:
                        if g.get("place_id"):
                            asset_url = await fetch_asset_location(session, asset_id, g["place_id"], ROBLOX_COOKIE)
                            if asset_url:
                                logger.info(f"Asset {asset_id} - URL obtida via fallback (PlaceID: {g['place_id']}).")
                                break
                        if g.get("universe_id"):
                            asset_url = await fetch_asset_location(session, asset_id, None, ROBLOX_COOKIE, g["universe_id"])
                            if asset_url:
                                logger.info(f"Asset {asset_id} - URL obtida via fallback (UniverseID: {g['universe_id']}).")
                                break
                else:
                    logger.warning(f"Asset {asset_id} - Nenhuma experiencia encontrada para o criador.")
            else:
                logger.error(f"Asset {asset_id} - Nao foi possivel obter o criador do asset para o fallback.")

    if not asset_url:
        logger.info(f"Asset {asset_id} - Tentando bypass de historico de versoes (forçado)...")
        asset_url = await fetch_version_fallback(session, asset_id, ROBLOX_COOKIE)

        if not asset_url and FALLBACK_GAMES:
            logger.info(
            f"Asset {asset_id} - Tentando {len(FALLBACK_GAMES)} jogos de fallback-games.txt..."
            )

        for place_id in FALLBACK_GAMES:
            test_url = await fetch_asset_location(
                session,
                asset_id,
                place_id,
                ROBLOX_COOKIE
            )

            if test_url:
                asset_url = test_url
                logger.info(
                    f"Asset {asset_id} - URL obtida via fallback-games.txt (PlaceID: {place_id})"
                )
                break

    if not asset_url:
        msg = f"Asset {asset_id} - URL de download inacessivel. O item provavelmente foi excluido permanentemente e não possui versões salvas."
        logger.error(msg)
        return None, msg

    try:
        logger.info(f"Asset URL: {asset_url}")
        async with session.get(asset_url) as response:
            if response.status != 200:
                msg = f"Asset {asset_id} - Falha no download HTTP {response.status}."
                logger.error(msg)
                return None, msg

            content_type = response.headers.get('Content-Type', '')
            if 'text/html' in content_type.lower() or 'application/json' in content_type.lower():
                msg = f"Asset {asset_id} - Arquivo invalido retornado (HTML/JSON de erro)."
                logger.error(msg)
                return None, msg

            content = await response.read()

            logger.info(f"Tamanho do arquivo: {len(content)} bytes")
            if len(content) == 0:
                msg = f"Asset {asset_id} - Arquivo vazio retornado."
                logger.error(msg)
                return None, msg

            final_ext = detect_file_extension(content, content_type, '.bin')

            logger.info(f"Content-Type: {content_type}")
            logger.info(f"Extensão detectada: {final_ext}")

            os.makedirs("downloaded_assets", exist_ok=True)
            file_path = os.path.join("downloaded_assets", f"{asset_id}_{sanitized_name}{final_ext}")
            
            with open(file_path, "wb") as f:
                f.write(content)
            
            if final_ext == '.m3u8':
                logger.info(f"Asset {asset_id} - Playlist HLS detectada. Iniciando reconstrução...")
                hls_webm_path = await process_hls_playlist(session, file_path, asset_url)
                if not hls_webm_path:
                    msg = f"Asset {asset_id} - Falha ao reconstruir video HLS."
                    logger.error(msg)
                    return None, msg
                file_path = hls_webm_path
                
            logger.info(f"Sucesso: {file_path}")
            return file_path, None
            
    except Exception as e:
        msg = f"Asset {asset_id} - Erro interno na conexao de download: {str(e)}"
        logger.error(msg)
        return None, msg

class FormatButton(discord.ui.Button):
    def __init__(self, label: str, fmt: str, row: int, is_audio: bool, style=discord.ButtonStyle.secondary):
        super().__init__(label=label, style=style, row=row)
        self.fmt = fmt
        self.is_audio = is_audio

    async def callback(self, interaction: discord.Interaction):
        if self.is_audio:
            self.view.audio_fmt = self.fmt
        else:
            self.view.video_fmt = self.fmt
            
        for child in self.view.children:
            if isinstance(child, FormatButton) and child.is_audio == self.is_audio:
                child.style = discord.ButtonStyle.primary if child.fmt == self.fmt else discord.ButtonStyle.secondary
                
        await interaction.response.edit_message(view=self.view)

class QualitySelect(discord.ui.Select):
    def __init__(self, is_audio: bool, row: int):
        self.is_audio = is_audio
        if is_audio:
            options = [
                discord.SelectOption(label="Original", value="original", description="Qualidade original"),
                discord.SelectOption(label="Alta", value="high", description="320kbps"),
                discord.SelectOption(label="Média", value="medium", description="192kbps"),
                discord.SelectOption(label="Baixa", value="low", description="128kbps"),
            ]
            placeholder = "Selecione a Qualidade de Áudio"
        else:
            options = [
                discord.SelectOption(label="Original", value="original", description="Resolução original"),
                discord.SelectOption(label="1080p", value="1080p"),
                discord.SelectOption(label="720p", value="720p"),
                discord.SelectOption(label="480p", value="480p"),
            ]
            placeholder = "Selecione a Qualidade de Vídeo"
        super().__init__(placeholder=placeholder, min_values=1, max_values=1, options=options, row=row)

    async def callback(self, interaction: discord.Interaction):
        if self.is_audio:
            self.view.audio_quality = self.values[0]
        else:
            self.view.video_quality = self.values[0]
        await interaction.response.defer()

class ConfirmButton(discord.ui.Button):
    def __init__(self, row: int):
        super().__init__(label="Confirmar e Processar", style=discord.ButtonStyle.success, row=row)

    async def callback(self, interaction: discord.Interaction):
        self.view.confirmed = True
        for child in self.view.children:
            child.disabled = True
        await interaction.response.edit_message(content=None, embed=discord.Embed(description="Processando conversão (FFmpeg)...", color=0x1446ff), view=self.view)
        self.view.stop()

class MediaFormatView(discord.ui.View):
    def __init__(self, has_audio: bool, has_video: bool):
        super().__init__(timeout=120)
        self.audio_fmt = '.ogg'
        self.video_fmt = '.webm'
        self.audio_quality = 'original'
        self.video_quality = 'original'
        self.confirmed = False
        
        row_idx = 0
        if has_audio:
            self.add_item(FormatButton("MP3", ".mp3", row=row_idx, is_audio=True))
            self.add_item(FormatButton("WAV", ".wav", row=row_idx, is_audio=True))
            self.add_item(FormatButton("OGG (Original)", ".ogg", row=row_idx, is_audio=True, style=discord.ButtonStyle.primary))
            row_idx += 1
            
        if has_video:
            self.add_item(FormatButton("MP4", ".mp4", row=row_idx, is_audio=False))
            self.add_item(FormatButton("MOV", ".mov", row=row_idx, is_audio=False))
            self.add_item(FormatButton("WEBM (Original)", ".webm", row=row_idx, is_audio=False, style=discord.ButtonStyle.primary))
            row_idx += 1

        if has_audio:
            self.add_item(QualitySelect(is_audio=True, row=row_idx))
            row_idx += 1

        if has_video:
            self.add_item(QualitySelect(is_audio=False, row=row_idx))
            row_idx += 1
            
        self.add_item(ConfirmButton(row=row_idx))

class RobloxAssetBot(discord.Client):
    def __init__(self):
        super().__init__(intents=discord.Intents.default())
        self.tree = app_commands.CommandTree(self)

    async def setup_hook(self):
        await self.tree.sync()

client = RobloxAssetBot()

@client.tree.command(name="asset", description="Baixa um unico asset do Roblox de forma segura")
async def asset(interaction: discord.Interaction, asset_id: str):
    await interaction.response.send_message(embed=discord.Embed(description="Processando...\n`🟩⬜️⬜️⬜️⬜️⬜️⬜️⬜️⬜️⬜`️", color=0x1446ff))
    
    async def progress_task():
        try:
            i = 1
            while i < 10:
                await asyncio.sleep(1)
                i += 1
                await interaction.edit_original_response(content=None, embed=discord.Embed(description=f"Processando...\n{'`🟩`' * i}{'`⬜`️' * (10 - i)}", color=0x1446ff))
        except asyncio.CancelledError:
            pass

    ptask = asyncio.create_task(progress_task())
    
    clean_id = asset_id.strip()
    
    async with aiohttp.ClientSession() as session:
        file_path, error = await download_core(session, clean_id)
        
    ptask.cancel()
    await interaction.edit_original_response(content=None, embed=discord.Embed(description="Processando...\n`🟩🟩🟩🟩🟩🟩🟩🟩🟩🟩`", color=0x1446ff))
        
    if file_path and os.path.exists(file_path):
        has_a = file_path.endswith('.ogg')
        has_v = file_path.endswith('.webm')
        
        if has_a or has_v:
            view = MediaFormatView(has_a, has_v)
            await interaction.edit_original_response(content=None, embed=discord.Embed(description="Mídia detectada! Selecione os formatos e qualidades:", color=0x1446ff), view=view)
            await view.wait()
            
            if view.confirmed:
                fmt = view.audio_fmt if has_a else view.video_fmt
                qual = view.audio_quality if has_a else view.video_quality
                file_path = await convert_media(file_path, fmt, qual)
            
            if os.path.getsize(file_path) > 10 * 1024 * 1024:
                await interaction.edit_original_response(content=None, embed=discord.Embed(description="O arquivo convertido excede o limite de 10MB do Discord. Enviando para o Litterbox...", color=0x1446ff), view=None)
                litterbox_url = await upload_litterbox(file_path)
                await interaction.edit_original_response(content=None, embed=discord.Embed(description=f"O arquivo excedeu o limite de 10MB do Discord. Link do Litterbox: {litterbox_url}", color=0x1446ff), view=None)
            else:
                await interaction.edit_original_response(content=None, embed=discord.Embed(description="Concluído!", color=0x1446ff), attachments=[discord.File(file_path)], view=None)
        else:
            if os.path.getsize(file_path) > 10 * 1024 * 1024:
                await interaction.edit_original_response(content=None, embed=discord.Embed(description="O arquivo excede o limite de 10MB do Discord. Enviando para o Litterbox...", color=0x1446ff))
                litterbox_url = await upload_litterbox(file_path)
                await interaction.edit_original_response(content=None, embed=discord.Embed(description=f"O arquivo excedeu o limite de 10MB do Discord. Link do Litterbox: {litterbox_url}", color=0x1446ff))
            else:
                await interaction.edit_original_response(content=None, embed=discord.Embed(description="Concluído!", color=0x1446ff), attachments=[discord.File(file_path)])
                
        try:
            if os.path.exists(file_path):
                os.remove(file_path)
        except Exception:
            pass
    else:
        await interaction.edit_original_response(content=None, embed=discord.Embed(description=f"Erro: {error}", color=0x1446ff))

@client.tree.command(name="assetbatch", description="Baixa multiplos assets e retorna um arquivo ZIP limpo")
async def assetbatch(interaction: discord.Interaction, asset_ids: str):
    await interaction.response.send_message(embed=discord.Embed(description="Processando...\n🟩⬜️⬜️⬜️⬜️⬜️⬜️⬜️⬜️⬜️", color=0x1446ff))
    
    async def progress_task():
        try:
            i = 1
            while i < 10:
                await asyncio.sleep(1.5)
                i += 1
                await interaction.edit_original_response(content=None, embed=discord.Embed(description=f"Processando...\n{'🟩' * i}{'⬜️' * (10 - i)}", color=0x1446ff))
        except asyncio.CancelledError:
            pass

    ptask = asyncio.create_task(progress_task())
    
    raw_ids = [x.strip() for x in asset_ids.split(',') if x.strip()]
    ids_list = []
    for x in raw_ids:
        if x not in ids_list:
            ids_list.append(x)
    
    if len(ids_list) > 20:
        ptask.cancel()
        await interaction.edit_original_response(content=None, embed=discord.Embed(description="Por favor, limite a 20 assets por lote para evitar sobrecarga.", color=0x1446ff))
        return

    downloaded_files = []
    errors = []
    failed_ids = []

    async with aiohttp.ClientSession() as session:
        results = []
        for aid in ids_list:
            try:
                res = await download_core(session, aid)
                results.append(res)
            except Exception as e:
                results.append(e)

    for aid, res in zip(ids_list, results):
        if isinstance(res, tuple):
            path, err = res
            if path:
                downloaded_files.append(path)
            else:
                failed_ids.append(aid)
                if err:
                    errors.append(err)
        else:
            failed_ids.append(aid)
            errors.append(f"Exceção severa: {str(res)}")

    ptask.cancel()
    await interaction.edit_original_response(content=None, embed=discord.Embed(description="Processando...\n🟩🟩🟩🟩🟩🟩🟩🟩🟩🟩", color=0x1446ff))

    if not downloaded_files:
        err_msg = "\n".join(errors)[:1800]
        await interaction.edit_original_response(content=None, embed=discord.Embed(description=f"Falha total no lote. Nenhum arquivo foi salvo.\nErros:\n{err_msg}", color=0x1446ff))
        return

    has_a = any(f.endswith('.ogg') for f in downloaded_files)
    has_v = any(f.endswith('.webm') for f in downloaded_files)

    if has_a or has_v:
        view = MediaFormatView(has_a, has_v)
        await interaction.edit_original_response(content=None, embed=discord.Embed(description="Mídias detectadas no lote! Selecione os formatos e qualidades:", color=0x1446ff), view=view)
        await view.wait()
        
        if view.confirmed:
            new_files = []
            for f in downloaded_files:
                if f.endswith('.ogg'):
                    f = await convert_media(f, view.audio_fmt, view.audio_quality)
                elif f.endswith('.webm'):
                    f = await convert_media(f, view.video_fmt, view.video_quality)
                new_files.append(f)
            downloaded_files = new_files
            await interaction.edit_original_response(content=None, embed=discord.Embed(description="Criando ZIP...", color=0x1446ff), view=None)
        else:
            await interaction.edit_original_response(content=None, embed=discord.Embed(description="Tempo esgotado. Mantendo os arquivos originais e criando ZIP...", color=0x1446ff), view=None)
    else:
        await interaction.edit_original_response(content=None, embed=discord.Embed(description="Criando ZIP...", color=0x1446ff))

    zip_filename = f"batch_{uuid.uuid4().hex[:8]}.zip"
    
    def create_zip():
        with zipfile.ZipFile(zip_filename, 'w', zipfile.ZIP_DEFLATED) as zipf:
            for file in downloaded_files:
                if os.path.exists(file):
                    zipf.write(file, os.path.basename(file))
                    
    await asyncio.to_thread(create_zip)

    final_msg = f"Lote concluido: {len(downloaded_files)} arquivos processados."
    if failed_ids:
        final_msg += f"\nFalhas ({len(failed_ids)}): "

        if len(failed_ids) == 1:
            final_msg += failed_ids[0]
        else:
            final_msg += ", ".join(f"`{i}`" for i in failed_ids)

    if os.path.exists(zip_filename):
        if os.path.getsize(zip_filename) > 10 * 1024 * 1024:
            await interaction.edit_original_response(content=None, embed=discord.Embed(description="O arquivo ZIP final excede o limite de 10MB do Discord. Enviando para o Litterbox...", color=0x1446ff))
            litterbox_url = await upload_litterbox(zip_filename)
            await interaction.edit_original_response(content=None, embed=discord.Embed(description=f"{final_msg}\n\nO arquivo ZIP excedeu o limite de 10MB do Discord. Link do Litterbox: {litterbox_url}", color=0x1446ff))
        else:
            await interaction.edit_original_response(content=None, embed=discord.Embed(description=final_msg, color=0x1446ff), attachments=[discord.File(zip_filename)])
            
        try:
            os.remove(zip_filename)
        except Exception:
            pass

    for file in downloaded_files:
        try:
            if os.path.exists(file):
                os.remove(file)
        except Exception:
            pass

client.run(DISCORD_TOKEN)