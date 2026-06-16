package main
import (
"archive/zip"
"bytes"
"context"
"encoding/json"
"fmt"
"io"
"math/rand"
"mime/multipart"
"net/http"
"net/url"
"os"
"os/exec"
"os/signal"
"path/filepath"
"regexp"
"strconv"
"strings"
"sync"
"syscall"
"time"
"github.com/bwmarrin/discordgo"
"github.com/google/uuid"
)
type SafeClient struct {
client *http.Client
}
var (
safeSem      = make(chan struct{}, 20)
safeRLMutex  sync.Mutex
safeLastReq  time.Time
safeInterval = 100 * time.Millisecond
safeCache    sync.Map
safeInflight sync.Map
)
type safeCacheItem struct {
resp *http.Response
body []byte
exp  time.Time
}
type safeCall struct {
wg   sync.WaitGroup
resp *http.Response
body []byte
err  error
}
func safeGlobalWait() {
safeRLMutex.Lock()
defer safeRLMutex.Unlock()
now := time.Now()
diff := now.Sub(safeLastReq)
if diff < safeInterval {
time.Sleep(safeInterval - diff)
}
safeLastReq = time.Now()
}
func safeCloneResponse(resp *http.Response, body []byte) *http.Response {
if resp == nil {
return nil
}
cloned := *resp
cloned.Body = io.NopCloser(bytes.NewBuffer(body))
return &cloned
}
func (s *SafeClient) executeReq(req *http.Request) (*http.Response, []byte, error) {
var reqBody []byte
isLarge := req.ContentLength > 5*1024*1024
if req.Body != nil && !isLarge {
reqBody, _ = io.ReadAll(req.Body)
}
baseDelay := 500 * time.Millisecond
maxRetries := 5
var lastResp *http.Response
var lastErr error
for i := 0; i < maxRetries; i++ {
safeGlobalWait()
if reqBody != nil {
req.Body = io.NopCloser(bytes.NewBuffer(reqBody))
} else if isLarge && i > 0 {
return nil, nil, fmt.Errorf("cannot retry request with large streamed body")
}
resp, err := s.client.Do(req)
if err != nil {
lastErr = err
time.Sleep(baseDelay + time.Duration(rand.Intn(500))*time.Millisecond)
baseDelay *= 2
continue
}
if resp.StatusCode == 429 {
ra := resp.Header.Get("Retry-After")
delay := baseDelay
if ra != "" {
if secs, err := strconv.Atoi(ra); err == nil {
delay = time.Duration(secs) * time.Second
} else if t, err := http.ParseTime(ra); err == nil {
delay = time.Until(t)
}
}
resp.Body.Close()
time.Sleep(delay + time.Duration(rand.Intn(500))*time.Millisecond)
baseDelay *= 2
continue
}
if resp.StatusCode >= 500 {
respBody, _ := io.ReadAll(resp.Body)
resp.Body.Close()
lastResp = safeCloneResponse(resp, respBody)
lastErr = fmt.Errorf("server error: %d", resp.StatusCode)
time.Sleep(baseDelay + time.Duration(rand.Intn(500))*time.Millisecond)
baseDelay *= 2
continue
}
respBody, err := io.ReadAll(resp.Body)
resp.Body.Close()
return safeCloneResponse(resp, respBody), respBody, err
}
if lastResp != nil {
return lastResp, nil, lastErr
}
return nil, nil, lastErr
}
func (s *SafeClient) Do(req *http.Request) (*http.Response, error) {
isCacheable := req.Method == "GET" || (req.Method == "POST" && req.ContentLength > 0 && req.ContentLength < 4096)
var cacheKey string
if isCacheable {
cacheKey = req.Method + ":" + req.URL.String()
if req.Method == "POST" && req.Body != nil {
bodyBytes, _ := io.ReadAll(req.Body)
req.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))
cacheKey += ":" + string(bodyBytes)
}
if val, ok := safeCache.Load(cacheKey); ok {
ci := val.(safeCacheItem)
if time.Now().Before(ci.exp) {
return safeCloneResponse(ci.resp, ci.body), nil
}
safeCache.Delete(cacheKey)
}
cInterface, loaded := safeInflight.LoadOrStore(cacheKey, &safeCall{})
c := cInterface.(*safeCall)
if !loaded {
c.wg.Add(1)
go func() {
safeSem <- struct{}{}
defer func() { <-safeSem }()
resp, body, err := s.executeReq(req)
c.resp = resp
c.body = body
c.err = err
if err == nil && resp != nil && len(body) < 512*1024 && (resp.StatusCode == 200 || resp.StatusCode == 400 || resp.StatusCode == 403) {
safeCache.Store(cacheKey, safeCacheItem{
resp: resp,
body: body,
exp:  time.Now().Add(1 * time.Minute),
})
}
c.wg.Done()
safeInflight.Delete(cacheKey)
}()
}
c.wg.Wait()
if c.err != nil && c.resp == nil {
return nil, c.err
}
return safeCloneResponse(c.resp, c.body), c.err
}
safeSem <- struct{}{}
defer func() { <-safeSem }()
resp, body, err := s.executeReq(req)
if err != nil && resp == nil {
return nil, err
}
return safeCloneResponse(resp, body), err
}
func (s *SafeClient) Get(urlStr string) (*http.Response, error) {
req, err := http.NewRequest("GET", urlStr, nil)
if err != nil {
return nil, err
}
return s.Do(req)
}
var (
discordToken = os.Getenv("DISCORD_TOKEN")
robloxCookie = os.Getenv("ROBLOX_COOKIE")
fallbackGames []int
assetTypes = map[int][2]string{
1: {"Image", ".png"}, 2: {"TShirt", ".png"}, 3: {"Audio", ".ogg"},
4: {"Mesh", ".mesh"}, 8: {"Hat", ".rbxm"}, 10: {"Model", ".rbxm"},
11: {"Shirt", ".png"}, 12: {"Pants", ".png"}, 13: {"Decal", ".png"},
17: {"Head", ".rbxm"}, 18: {"Face", ".png"}, 19: {"Gear", ".rbxm"},
21: {"Badge", ".png"}, 24: {"Animation", ".rbxm"}, 27: {"Torso", ".rbxm"},
28: {"RightArm", ".rbxm"}, 29: {"LeftArm", ".rbxm"}, 32: {"Package", ".rbxm"},
34: {"GamePass", ".png"}, 38: {"Plugin", ".rbxm"}, 40: {"MeshPart", ".mesh"},
41: {"HairAccessory", ".rbxm"}, 42: {"FaceAccessory", ".rbxm"}, 43: {"NeckAccessory", ".rbxm"},
44: {"ShoulderAccessory", ".rbxm"}, 45: {"FrontAccessory", ".rbxm"}, 46: {"BackAccessory", ".rbxm"},
47: {"WaistAccessory", ".rbxm"}, 57: {"EarAccessory", ".rbxm"}, 58: {"EyeAccessory", ".rbxm"},
61: {"EmoteAnimation", ".rbxm"}, 62: {"Video", ".webm"}, 64: {"TShirtAccessory", ".rbxm"},
65: {"ShirtAccessory", ".rbxm"}, 66: {"PantsAccessory", ".rbxm"}, 67: {"JacketAccessory", ".rbxm"},
68: {"SweaterAccessory", ".rbxm"}, 69: {"ShortsAccessory", ".rbxm"}, 70: {"DressSkirtAccessory", ".rbxm"},
73: {"FontFamily", ".json"}, 76: {"EyebrowAccessory", ".rbxm"}, 77: {"EyelashAccessory", ".rbxm"},
79: {"DynamicHead", ".rbxm"},
}
noBinaryTypes = []int{21, 34}
httpClient = &SafeClient{client: &http.Client{Timeout: 60 * time.Second}}
activeViews sync.Map
)
const (
colorCyan   = "\033[36m"
colorGreen  = "\033[32m"
colorYellow = "\033[33m"
colorRed    = "\033[31m"
colorBright = "\033[1;31m"
colorReset  = "\033[0m"
)
func logDebug(msg string, args ...interface{}) {
fmt.Printf("%s - %sDEBUG%s - %s\n", time.Now().Format("15:04:05"), colorCyan, colorReset, fmt.Sprintf(msg, args...))
}
func logInfo(msg string, args ...interface{}) {
fmt.Printf("%s - %sINFO%s - %s\n", time.Now().Format("15:04:05"), colorGreen, colorReset, fmt.Sprintf(msg, args...))
}
func logWarning(msg string, args ...interface{}) {
fmt.Printf("%s - %sWARNING%s - %s\n", time.Now().Format("15:04:05"), colorYellow, colorReset, fmt.Sprintf(msg, args...))
}
func logError(msg string, args ...interface{}) {
fmt.Printf("%s - %sERROR%s - %s\n", time.Now().Format("15:04:05"), colorRed, colorReset, fmt.Sprintf(msg, args...))
}
func loadFallbackGames() []int {
var placeIds []int
data, err := os.ReadFile("fallback-games.txt")
if err != nil {
return placeIds
}
lines := strings.Split(string(data), "\n")
for _, line := range lines {
line = strings.TrimSpace(line)
if line == "" || strings.HasPrefix(line, "#") {
continue
}
parts := strings.SplitN(line, "#", 2)
placeIdStr := strings.TrimSpace(parts[0])
if id, err := strconv.Atoi(placeIdStr); err == nil {
placeIds = append(placeIds, id)
}
}
return placeIds
}
func uploadLitterbox(filePath string, expire string) string {
urlStr := "https://litterbox.catbox.moe/resources/internals/api.php"
file, err := os.Open(filePath)
if err != nil {
return fmt.Sprintf("Erro: %v", err)
}
defer file.Close()
body := &bytes.Buffer{}
writer := multipart.NewWriter(body)
_ = writer.WriteField("reqtype", "fileupload")
_ = writer.WriteField("time", expire)
part, err := writer.CreateFormFile("fileToUpload", filepath.Base(filePath))
if err != nil {
return fmt.Sprintf("Erro: %v", err)
}
_, _ = io.Copy(part, file)
writer.Close()
req, err := http.NewRequest("POST", urlStr, body)
if err != nil {
return fmt.Sprintf("Erro: %v", err)
}
req.Header.Set("Content-Type", writer.FormDataContentType())
resp, err := httpClient.Do(req)
if err != nil {
return fmt.Sprintf("Erro: %v", err)
}
defer resp.Body.Close()
respBody, _ := io.ReadAll(resp.Body)
if resp.StatusCode == 200 {
return string(respBody)
}
return fmt.Sprintf("Erro: HTTP %d", resp.StatusCode)
}
func detectFileExtension(content []byte, contentType string, fallbackExt string) string {
if bytes.HasPrefix(content, []byte("#EXTM3U")) {
return ".m3u8"
}
if bytes.HasPrefix(content, []byte("\x89PNG\r\n\x1a\n")) {
return ".png"
}
if bytes.HasPrefix(content, []byte("OggS")) {
return ".ogg"
}
if bytes.HasPrefix(content, []byte("\x1aE\xdf\xa3")) {
return ".webm"
}
if bytes.HasPrefix(content, []byte("<roblox!")) {
return ".rbxm"
}
if bytes.HasPrefix(content, []byte("<roblox")) {
return ".rbxmx"
}
if bytes.HasPrefix(content, []byte("version ")) {
return ".mesh"
}
if bytes.HasPrefix(content, []byte("{"")) || bytes.HasPrefix(content, []byte("[")) {
return ".json"
}
ctype := strings.ToLower(contentType)
if strings.Contains(ctype, "image/png") {
return ".png"
}
if strings.Contains(ctype, "audio/ogg") {
return ".ogg"
}
if strings.Contains(ctype, "video/webm") {
return ".webm"
}
if strings.Contains(ctype, "application/xml") {
return ".rbxmx"
}
if strings.Contains(ctype, "application/json") {
return ".json"
}
if strings.Contains(ctype, "text/plain") {
return ".txt"
}
return fallbackExt
}
func fetchCreatorGames(creatorID int, creatorType string) []int {
var placeIds []int
var urlStr string
if creatorType == "Group" {
urlStr = fmt.Sprintf("https://games.roproxy.com/v2/groups/%d/games?accessFilter=2&sortOrder=Asc&limit=50", creatorID)
} else {
urlStr = fmt.Sprintf("https://games.roproxy.com/v2/users/%d/games?accessFilter=2&sortOrder=Asc&limit=50", creatorID)
}
resp, err := httpClient.Get(urlStr)
if err != nil {
logWarning("Falha ao buscar experiencias do criador %d: %v", creatorID, err)
return placeIds
}
defer resp.Body.Close()
if resp.StatusCode == 200 {
var data struct {
Data []struct {
RootPlace struct {
ID int json:"id"
} json:"rootPlace"
} json:"data"
}
if err := json.NewDecoder(resp.Body).Decode(&data); err == nil {
for _, game := range data.Data {
if game.RootPlace.ID != 0 {
placeIds = append(placeIds, game.RootPlace.ID)
}
}
}
}
return placeIds
}
func fetchAssetDetails(assetID string, maxRetries int) map[string]interface{} {
urlStr := fmt.Sprintf("https://economy.roproxy.com/v2/assets/%s/details", assetID)
for attempt := 0; attempt < maxRetries; attempt++ {
resp, err := httpClient.Get(urlStr)
if err != nil {
time.Sleep(500 * time.Millisecond)
continue
}
defer resp.Body.Close()
if resp.StatusCode == 200 || resp.StatusCode == 400 || resp.StatusCode == 403 {
var data map[string]interface{}
if err := json.NewDecoder(resp.Body).Decode(&data); err == nil {
return data
}
} else if resp.StatusCode == 429 {
time.Sleep(time.Duration(500*(attempt+1)) * time.Millisecond)
continue
} else {
break
}
}
return nil
}
func fetchAssetLocation(assetID string, assetType string, placeID int, cookie string) string {
urlStr := "https://assetdelivery.roproxy.com/v2/assets/batch"
bodyArray := []map[string]interface{}{
{
"assetId":   assetID,
"assetType": assetType,
"requestId": "0",
},
}
bodyBytes, _ := json.Marshal(bodyArray)
req, _ := http.NewRequest("POST", urlStr, bytes.NewBuffer(bodyBytes))
req.Header.Set("User-Agent", "Roblox/WinInet")
req.Header.Set("Content-Type", "application/json")
req.Header.Set("Accept", "*/*")
req.Header.Set("Roblox-Browser-Asset-Request", "false")
if cookie != "" {
req.Header.Set("Cookie", fmt.Sprintf(".ROBLOSECURITY=%s", cookie))
}
if placeID != 0 {
req.Header.Set("Roblox-Place-Id", strconv.Itoa(placeID))
}
resp, err := httpClient.Do(req)
if err != nil {
logDebug("Erro ao buscar localizacao do asset %s (Place: %d): %v", assetID, placeID, err)
return ""
}
defer resp.Body.Close()
if resp.StatusCode == 200 {
var locations []struct {
Locations []struct {
Location string json:"location"
} json:"locations"
}
if err := json.NewDecoder(resp.Body).Decode(&locations); err == nil {
if len(locations) > 0 && len(locations[0].Locations) > 0 {
return locations[0].Locations[0].Location
}
}
}
return ""
}
func sanitizeFilename(name string) string {
re := regexp.MustCompile([\\/*?"<>|])
sanitized := re.ReplaceAllString(name, "")
return strings.ReplaceAll(sanitized, " ", "_")
}
func convertMedia(inputPath string, format string) string {
if format == "" || strings.HasSuffix(inputPath, format) {
return inputPath
}
inputDir := filepath.Dir(inputPath)
if inputDir == "" {
inputDir = "."
}
inputName := filepath.Base(inputPath)
outputName := strings.TrimSuffix(inputName, filepath.Ext(inputName)) + format
outputPath := filepath.Join(inputDir, outputName)
var cmdArgs []string
if format == ".mp3" {
cmdArgs = []string{"-y", "-i", inputName, "-c:a", "libmp3lame", "-q:a", "2", outputName}
} else if format == ".wav" {
cmdArgs = []string{"-y", "-i", inputName, "-c:a", "pcm_s16le", outputName}
} else if format == ".mp4" || format == ".mov" {
cmdArgs = []string{"-y", "-i", inputName, outputName}
} else {
return inputPath
}
cmd := exec.Command("ffmpeg", cmdArgs...)
cmd.Dir = inputDir
out, err := cmd.CombinedOutput()
if len(out) > 0 {
logInfo(string(out))
}
if err != nil {
logError("Erro no FFmpeg: %v", err)
logInfo("FFmpeg return code: %d", cmd.ProcessState.ExitCode())
return inputPath
}
logInfo("FFmpeg return code: 0")
info, statErr := os.Stat(outputPath)
if statErr == nil && info.Size() > 0 {
os.Remove(inputPath)
return outputPath
}
return inputPath
}
func getURLWithAuth(basePath, targetPath, masterURL string) string {
joinedURL, err := url.Parse(basePath)
if err != nil {
return targetPath
}
targetParsed, err := url.Parse(targetPath)
if err != nil {
return targetPath
}
finalURL := joinedURL.ResolveReference(targetParsed)
masterParsed, _ := url.Parse(masterURL)
if targetParsed.RawQuery == "" && finalURL.Host == masterParsed.Host {
finalURL.RawQuery = masterParsed.RawQuery
}
return finalURL.String()
}
func processHLSPlaylist(m3u8Path string, baseURL string) string {
logInfo("Processando playlist HLS: %s", m3u8Path)
content, err := os.ReadFile(m3u8Path)
if err != nil {
logError("Erro geral processando HLS: %v", err)
return ""
}
m3u8Content := string(content)
lines := strings.Split(m3u8Content, "\n")
count := min(len(lines), 5)
logInfo("Tipo de playlist detectada. Primeiras linhas: %v", lines[:count])
rbxBaseURI := ""
reRbxBase := regexp.MustCompile(#EXT-X-DEFINE:NAME="RBX-BASE-URI",VALUE="([^"]+)")
for _, line := range lines {
match := reRbxBase.FindStringSubmatch(line)
if len(match) > 1 {
rbxBaseURI = match[1]
if !strings.HasSuffix(rbxBaseURI, "/") {
rbxBaseURI += "/"
}
logInfo("RBX-BASE-URI detectado: %s", rbxBaseURI)
break
}
}
bestPlaylistURL := ""
var streams [][2]string
for i, line := range lines {
if strings.HasPrefix(line, "#EXT-X-STREAM-INF") {
if i+1 < len(lines) {
streams = append(streams, [2]string{line, lines[i+1]})
}
}
}
logInfo("Quantidade de streams encontrados: %d", len(streams))
if len(streams) > 0 {
var bestStream [2]string
maxHeight := -1
reRes := regexp.MustCompile(RESOLUTION=\d+x(\d+))
for _, stream := range streams {
match := reRes.FindStringSubmatch(stream[0])
if len(match) > 1 {
height, _ := strconv.Atoi(match[1])
if height > maxHeight {
maxHeight = height
bestStream = stream
}
}
}
if maxHeight != -1 {
bestPlaylistURL = bestStream[1]
logInfo("Stream selecionado (Maior Resolução): %s", bestStream[0])
} else {
bestPlaylistURL = streams[0][1]
for _, stream := range streams {
if strings.Contains(stream[0], "720") || strings.Contains(stream[1], "720") {
bestPlaylistURL = stream[1]
bestStream = stream
break
}
}
if bestStream[0] == "" {
bestStream = streams[0]
}
logInfo("Stream selecionado (Fallback): %s", bestStream[0])
}
}
reqHeaders := map[string]string{
"User-Agent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/122.0.0.0 Safari/537.36",
}
internalM3u8Content := m3u8Content
if bestPlaylistURL != "" {
if strings.Contains(bestPlaylistURL, "{$RBX-BASE-URI}") && rbxBaseURI != "" {
bestPlaylistURL = strings.ReplaceAll(bestPlaylistURL, "{$RBX-BASE-URI}", strings.TrimSuffix(rbxBaseURI, "/"))
} else {
bestPlaylistURL = getURLWithAuth(baseURL, bestPlaylistURL, baseURL)
}
logInfo("URL INTERNA = %s", bestPlaylistURL)
req, _ := http.NewRequest("GET", bestPlaylistURL, nil)
for k, v := range reqHeaders {
req.Header.Set(k, v)
}
resp, err := httpClient.Do(req)
if err != nil || resp.StatusCode != 200 {
status := 0
if resp != nil {
status = resp.StatusCode
}
logError("Falha ao baixar playlist interna: %d", status)
return ""
}
body, _ := io.ReadAll(resp.Body)
resp.Body.Close()
internalM3u8Content = string(body)
} else {
bestPlaylistURL = baseURL
}
var segments []string
for _, line := range strings.Split(internalM3u8Content, "\n") {
line = strings.TrimSpace(line)
if line != "" && !strings.HasPrefix(line, "#") {
segments = append(segments, line)
}
}
if len(segments) == 0 {
logError("Nenhum segmento encontrado na playlist HLS.")
return ""
}
outputDir := filepath.Dir(m3u8Path)
if outputDir == "" {
outputDir = "."
}
baseName := strings.TrimSuffix(filepath.Base(m3u8Path), filepath.Ext(m3u8Path))
var segmentFiles []string
logInfo("Quantidade de segmentos encontrados: %d", len(segments))
logInfo("Baixando %d segmentos HLS para %s...", len(segments), baseName)
segmentsBasePath := bestPlaylistURL
for i, seg := range segments {
segURL := getURLWithAuth(segmentsBasePath, seg, baseURL)
cleanURL := strings.Split(segURL, "?")[0]
filename := filepath.Base(cleanURL)
ext := ".webm"
if strings.Contains(filename, ".") {
ext = filepath.Ext(filename)
}
segPath := filepath.Join(outputDir, fmt.Sprintf("%s_seg_%04d%s", baseName, i, ext))
req, _ := http.NewRequest("GET", segURL, nil)
for k, v := range reqHeaders {
req.Header.Set(k, v)
}
resp, err := httpClient.Do(req)
if err == nil && resp.StatusCode == 200 {
content, _ := io.ReadAll(resp.Body)
resp.Body.Close()
os.WriteFile(segPath, content, 0644)
segmentFiles = append(segmentFiles, segPath)
logInfo("Segmento %04d baixado | Extensão: %s | Tamanho: %d bytes", i, ext, len(content))
} else {
status := 0
if resp != nil {
status = resp.StatusCode
resp.Body.Close()
}
logError("Falha ao baixar segmento HLS %s (HTTP %d)", cleanURL, status)
}
}
if len(segmentFiles) == 0 {
return ""
}
listName := fmt.Sprintf("%s_list.txt", baseName)
listPath := filepath.Join(outputDir, listName)
var listContent strings.Builder
for _, sf := range segmentFiles {
listContent.WriteString(fmt.Sprintf("file '%s'\n", filepath.Base(sf)))
}
os.WriteFile(listPath, []byte(listContent.String()), 0644)
webmName := fmt.Sprintf("%s.webm", baseName)
webmOutput := filepath.Join(outputDir, webmName)
logInfo("Concatenando segmentos em %s...", webmName)
cmd := exec.Command("ffmpeg", "-y", "-f", "concat", "-safe", "0", "-i", listName, "-c", "copy", webmName)
cmd.Dir = outputDir
_, err = cmd.CombinedOutput()
if err != nil {
logError("Falha na reconstrução HLS.")
logError("Motivo: FFmpeg falhou com código de retorno %d", cmd.ProcessState.ExitCode())
return ""
}
logInfo("Resultado final da concatenação HLS: Sucesso. Salvo em %s", webmOutput)
os.Remove(m3u8Path)
os.Remove(listPath)
for _, sf := range segmentFiles {
os.Remove(sf)
}
return webmOutput
}
func fetchVersionFallback(assetID string, cookie string, maxVersions int) string {
for version := 1; version <= maxVersions; version++ {
urlStr := fmt.Sprintf("https://assetdelivery.roproxy.com/v1/asset/?id=%s&version=%d", assetID, version)
req, _ := http.NewRequest("GET", urlStr, nil)
req.Header.Set("User-Agent", "Roblox/WinInet")
req.Header.Set("Roblox-Browser-Asset-Request", "false")
if cookie != "" {
req.Header.Set("Cookie", fmt.Sprintf(".ROBLOSECURITY=%s", cookie))
}
client := &SafeClient{client: &http.Client{
CheckRedirect: func(req *http.Request, via []*http.Request) error {
return nil
},
}}
resp, err := client.Do(req)
if err == nil && resp.StatusCode == 200 {
contentType := resp.Header.Get("Content-Type")
if !strings.Contains(strings.ToLower(contentType), "text/html") && !strings.Contains(strings.ToLower(contentType), "application/json") {
logInfo("Asset %s - Sucesso ao recuperar a versao %d que escapou da moderacao!", assetID, version)
resp.Body.Close()
return urlStr
}
resp.Body.Close()
} else if resp != nil {
resp.Body.Close()
}
time.Sleep(500 * time.Millisecond)
}
return ""
}
func downloadCore(assetID string) (string, string) {
details := fetchAssetDetails(assetID, 10)
assetName := assetID
assetTypeID := 0
creatorID := 0
creatorType := ""
targetAssetTypeStr := "Unknown"
expectedExtension := ".bin"
if details != nil && details["errors"] == nil {
if name, ok := details["Name"].(string); ok {
assetName = name
}
if typeId, ok := details["AssetTypeId"].(float64); ok {
assetTypeID = int(typeId)
}
if creator, ok := details["Creator"].(map[string]interface{}); ok {
if cid, ok := creator["CreatorTargetId"].(float64); ok {
creatorID = int(cid)
}
if ctype, ok := creator["CreatorType"].(string); ok {
creatorType = ctype
}
}
if typeInfo, exists := assetTypes[assetTypeID]; exists {
targetAssetTypeStr = typeInfo[0]
expectedExtension = typeInfo[1]
} else {
targetAssetTypeStr = "Model"
expectedExtension = ".bin"
}
} else {
logWarning("Asset %s - Detalhes negados (provavelmente moderado). Forcando bypass direto...", assetID)
}
sanitizedName := sanitizeFilename(assetName)
logInfo("Processando Asset %s | Nome: %s | TypeID: %d (%s)", assetID, sanitizedName, assetTypeID, targetAssetTypeStr)
for _, nbt := range noBinaryTypes {
if assetTypeID == nbt {
msg := fmt.Sprintf("Asset %s e do tipo sem arquivo binario (%s).", assetID, targetAssetTypeStr)
logWarning(msg)
return "", msg
}
}
assetURL := ""
if assetTypeID != 0 {
logInfo("Asset %s - Tentando obter URL de forma publica...", assetID)
assetURL = fetchAssetLocation(assetID, targetAssetTypeStr, 0, "")
if assetURL != "" {
logInfo("Asset %s - URL publica obtida com sucesso!", assetID)
} else {
logInfo("Asset %s - Acesso publico negado. Tentando fallback com PlaceIds e Cookie...", assetID)
if creatorID != 0 {
placeIds := fetchCreatorGames(creatorID, creatorType)
if len(placeIds) > 0 {
for _, pid := range placeIds {
assetURL = fetchAssetLocation(assetID, targetAssetTypeStr, pid, robloxCookie)
if assetURL != "" {
logInfo("Asset %s - URL obtida via fallback (PlaceID: %d).", assetID, pid)
break
}
}
} else {
logWarning("Asset %s - Nenhuma experiencia encontrada para o criador.", assetID)
}
} else {
logError("Asset %s - Nao foi possivel obter o criador do asset para o fallback.", assetID)
}
}
}
if assetURL == "" {
logInfo("Asset %s - Tentando bypass de historico de versoes (forçado)...", assetID)
assetURL = fetchVersionFallback(assetID, robloxCookie, 10)
if assetURL == "" && len(fallbackGames) > 0 {
logInfo("Asset %s - Tentando %d jogos de fallback-games.txt...", assetID, len(fallbackGames))
}
if assetURL == "" {
for _, placeID := range fallbackGames {
testURL := fetchAssetLocation(assetID, targetAssetTypeStr, placeID, robloxCookie)
if testURL != "" {
assetURL = testURL
logInfo("Asset %s - URL obtida via fallback-games.txt (PlaceID: %d)", assetID, placeID)
break
}
}
}
}
if assetURL == "" {
msg := fmt.Sprintf("Asset %s - URL de download inacessivel. O item provavelmente foi excluido permanentemente e não possui versões salvas.", assetID)
logError(msg)
return "", msg
}
logInfo("Asset URL: %s", assetURL)
resp, err := httpClient.Get(assetURL)
if err != nil {
msg := fmt.Sprintf("Asset %s - Erro interno na conexao de download: %v", assetID, err)
logError(msg)
return "", msg
}
defer resp.Body.Close()
if resp.StatusCode != 200 {
msg := fmt.Sprintf("Asset %s - Falha no download HTTP %d.", assetID, resp.StatusCode)
logError(msg)
return "", msg
}
contentType := resp.Header.Get("Content-Type")
if strings.Contains(strings.ToLower(contentType), "text/html") || strings.Contains(strings.ToLower(contentType), "application/json") {
msg := fmt.Sprintf("Asset %s - Arquivo invalido retornado (HTML/JSON de erro).", assetID)
logError(msg)
return "", msg
}
content, err := io.ReadAll(resp.Body)
if err != nil {
msg := fmt.Sprintf("Asset %s - Erro ao ler corpo da resposta: %v", assetID, err)
logError(msg)
return "", msg
}
logInfo("Tamanho do arquivo: %d bytes", len(content))
if len(content) == 0 {
msg := fmt.Sprintf("Asset %s - Arquivo vazio retornado.", assetID)
logError(msg)
return "", msg
}
finalExt := detectFileExtension(content, contentType, expectedExtension)
logInfo("Content-Type: %s", contentType)
logInfo("Extensão detectada: %s", finalExt)
os.MkdirAll("downloaded_assets", os.ModePerm)
filePath := filepath.Join("downloaded_assets", fmt.Sprintf("%s_%s%s", assetID, sanitizedName, finalExt))
err = os.WriteFile(filePath, content, 0644)
if err != nil {
msg := fmt.Sprintf("Asset %s - Erro ao salvar arquivo: %v", assetID, err)
logError(msg)
return "", msg
}
if finalExt == ".m3u8" {
logInfo("Asset %s - Playlist HLS detectada. Iniciando reconstrução...", assetID)
hlsWebmPath := processHLSPlaylist(filePath, assetURL)
if hlsWebmPath == "" {
msg := fmt.Sprintf("Asset %s - Falha ao reconstruir video HLS.", assetID)
logError(msg)
return "", msg
}
filePath = hlsWebmPath
}
info, err := os.Stat(filePath)
if err != nil || info.Size() == 0 {
os.Remove(filePath)
msg := fmt.Sprintf("Asset %s - Download resultou em arquivo vazio ou inacessível.", assetID)
logError(msg)
return "", msg
}
logInfo("Sucesso: %s", filePath)
return filePath, ""
}
type ViewState struct {
AudioFmt  string
VideoFmt  string
Confirmed bool
Done      chan struct{}
}
func getFormatButtons(uuidStr string, audioFmt string, videoFmt string, hasAudio bool, hasVideo bool) []discordgo.MessageComponent {
var rows []discordgo.MessageComponent
if hasAudio {
row := discordgo.ActionsRow{}
audioFormats := []struct {
Label string
Fmt   string
}{
{"MP3", ".mp3"},
{"WAV", ".wav"},
{"OGG (Original)", ".ogg"},
}
for *, af := range audioFormats {
style := discordgo.SecondaryButton
if audioFmt == af.Fmt {
style = discordgo.PrimaryButton
}
row.Components = append(row.Components, discordgo.Button{
Label:    af.Label,
Style:    style,
CustomID: "fmt_audio*" + af.Fmt + "_" + uuidStr,
})
}
rows = append(rows, row)
}
if hasVideo {
row := discordgo.ActionsRow{}
videoFormats := []struct {
Label string
Fmt   string
}{
{"MP4", ".mp4"},
{"MOV", ".mov"},
{"WEBM (Original)", ".webm"},
}
for *, vf := range videoFormats {
style := discordgo.SecondaryButton
if videoFmt == vf.Fmt {
style = discordgo.PrimaryButton
}
row.Components = append(row.Components, discordgo.Button{
Label:    vf.Label,
Style:    style,
CustomID: "fmt_video*" + vf.Fmt + "_" + uuidStr,
})
}
rows = append(rows, row)
}
confirmRow := discordgo.ActionsRow{
Components: []discordgo.MessageComponent{
discordgo.Button{
Label:    "Confirmar e Processar",
Style:    discordgo.SuccessButton,
CustomID: "confirm_" + uuidStr,
},
},
}
rows = append(rows, confirmRow)
return rows
}
func handleInteractionCreate(s *discordgo.Session, i *discordgo.InteractionCreate) {
if i.Type == discordgo.InteractionApplicationCommand {
data := i.ApplicationCommandData()
if data.Name == "asset" {
s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
Type: discordgo.InteractionResponseChannelMessageWithSource,
Data: &discordgo.InteractionResponseData{
Content: "Processando...\n🟩",
},
})
ctx, cancel := context.WithCancel(context.Background())
go func() {
squares := 1
for {
select {
case <-ctx.Done():
return
case <-time.After(1 * time.Second):
squares++
if squares >= 10 {
return
}
msg := "Processando...\n" + strings.Repeat("🟩", squares)
s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &msg})
}
}
}()
assetID := data.Options[0].StringValue()
cleanID := strings.TrimSpace(assetID)
filePath, errStr := downloadCore(cleanID)
cancel()
msg := "Processando...\n🟩🟩🟩🟩🟩🟩🟩🟩🟩🟩"
s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &msg})
if filePath != "" && errStr == "" {
hasA := strings.HasSuffix(filePath, ".ogg")
hasV := strings.HasSuffix(filePath, ".webm")
if hasA || hasV {
sessionID := uuid.New().String()
view := &ViewState{
AudioFmt: ".ogg",
VideoFmt: ".webm",
Done:     make(chan struct{}),
}
activeViews.Store(sessionID, view)
msg := "Mídia detectada! Selecione o formato desejado:"
components := getFormatButtons(sessionID, view.AudioFmt, view.VideoFmt, hasA, hasV)
s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
Content:    &msg,
Components: &components,
})
select {
case <-view.Done:
case <-time.After(120 * time.Second):
}
activeViews.Delete(sessionID)
if view.Confirmed {
fmtToUse := view.VideoFmt
if hasA {
fmtToUse = view.AudioFmt
}
filePath = convertMedia(filePath, fmtToUse)
}
}
info, _ := os.Stat(filePath)
if info.Size() > 10*1024*1024 {
msg := "O arquivo convertido excede o limite de 10MB do Discord. Enviando para o Litterbox..."
s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &msg, Components: &[]discordgo.MessageComponent{}})
litterboxURL := uploadLitterbox(filePath, "72h")
msg = fmt.Sprintf("O arquivo excedeu o limite de 10MB do Discord. Link do Litterbox: %s", litterboxURL)
s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &msg})
} else {
fileReader, _ := os.Open(filePath)
defer fileReader.Close()
msg := "Concluído!"
s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
Content:    &msg,
Components: &[]discordgo.MessageComponent{},
Files: []*discordgo.File{
{
Name:        filepath.Base(filePath),
ContentType: "application/octet-stream",
Reader:      fileReader,
},
},
})
}
os.Remove(filePath)
} else {
msg := fmt.Sprintf("Erro: %s", errStr)
s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &msg})
}
}
if data.Name == "assetbatch" {
s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
Type: discordgo.InteractionResponseChannelMessageWithSource,
Data: &discordgo.InteractionResponseData{
Content: "Processando...\n🟩",
},
})
ctx, cancel := context.WithCancel(context.Background())
go func() {
squares := 1
for {
select {
case <-ctx.Done():
return
case <-time.After(1500 * time.Millisecond):
squares++
if squares >= 10 {
return
}
msg := "Processando...\n" + strings.Repeat("🟩", squares)
s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &msg})
}
}
}()
assetIDsRaw := data.Options[0].StringValue()
var idsList []string
for _, x := range strings.Split(assetIDsRaw, ",") {
clean := strings.TrimSpace(x)
if clean != "" {
idsList = append(idsList, clean)
}
}
if len(idsList) > 20 {
cancel()
msg := "Por favor, limite a 20 assets por lote para evitar sobrecarga."
s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &msg})
return
}
var downloadedFiles []string
var errors []string
var failedIds []string
for _, aid := range idsList {
path, errStr := downloadCore(aid)
if path != "" && errStr == "" {
if info, err := os.Stat(path); err == nil && info.Size() > 0 {
downloadedFiles = append(downloadedFiles, path)
} else {
failedIds = append(failedIds, aid)
errors = append(errors, "Arquivo resultante vazio ou corrompido.")
os.Remove(path)
}
} else {
failedIds = append(failedIds, aid)
if errStr != "" {
errors = append(errors, errStr)
}
}
}
cancel()
msg := "Processando...\n🟩🟩🟩🟩🟩🟩🟩🟩🟩🟩"
s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &msg})
if len(downloadedFiles) == 0 {
errsJoined := strings.Join(errors, "\n")
if len(errsJoined) > 1800 {
errsJoined = errsJoined[:1800]
}
msg := fmt.Sprintf("Falha total no lote. Nenhum arquivo foi salvo.\nErros:\n%s", errsJoined)
s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &msg})
return
}
hasA := false
hasV := false
for _, f := range downloadedFiles {
if strings.HasSuffix(f, ".ogg") {
hasA = true
}
if strings.HasSuffix(f, ".webm") {
hasV = true
}
}
if hasA || hasV {
sessionID := uuid.New().String()
view := &ViewState{
AudioFmt: ".ogg",
VideoFmt: ".webm",
Done:     make(chan struct{}),
}
activeViews.Store(sessionID, view)
msg := "Mídias detectadas no lote! Selecione os formatos:"
components := getFormatButtons(sessionID, view.AudioFmt, view.VideoFmt, hasA, hasV)
s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
Content:    &msg,
Components: &components,
})
select {
case <-view.Done:
case <-time.After(120 * time.Second):
}
activeViews.Delete(sessionID)
if view.Confirmed {
msg := "Criando ZIP..."
s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &msg, Components: &[]discordgo.MessageComponent{}})
var newFiles []string
for _, f := range downloadedFiles {
if strings.HasSuffix(f, ".ogg") {
f = convertMedia(f, view.AudioFmt)
} else if strings.HasSuffix(f, ".webm") {
f = convertMedia(f, view.VideoFmt)
}
newFiles = append(newFiles, f)
}
downloadedFiles = newFiles
} else {
msg := "Tempo esgotado. Mantendo os arquivos originais e criando ZIP..."
s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &msg, Components: &[]discordgo.MessageComponent{}})
}
} else {
msg := "Criando ZIP..."
s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &msg})
}
zipFilename := fmt.Sprintf("batch_%s.zip", uuid.New().String()[:8])
zipFile, _ := os.Create(zipFilename)
zipWriter := zip.NewWriter(zipFile)
for _, file := range downloadedFiles {
if info, err := os.Stat(file); err == nil && info.Size() > 0 {
fWriter, _ := zipWriter.Create(filepath.Base(file))
b, _ := os.ReadFile(file)
fWriter.Write(b)
}
}
zipWriter.Close()
zipFile.Close()
finalMsg := fmt.Sprintf("Lote concluido: %d arquivos processados.", len(downloadedFiles))
if len(failedIds) > 0 {
finalMsg += fmt.Sprintf("\nFalhas (%d): ", len(failedIds))
if len(failedIds) == 1 {
finalMsg += failedIds[0]
} else {
var fidsQuoted []string
for _, id := range failedIds {
fidsQuoted = append(fidsQuoted, fmt.Sprintf("%s", id))
}
finalMsg += strings.Join(fidsQuoted, ", ")
}
}
if info, err := os.Stat(zipFilename); err == nil {
if info.Size() > 10*1024*1024 {
msg := "O arquivo ZIP final excede o limite de 10MB do Discord. Enviando para o Litterbox..."
s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &msg})
litterboxURL := uploadLitterbox(zipFilename, "72h")
msg = fmt.Sprintf("%s\n\nO arquivo ZIP excedeu o limite de 10MB do Discord. Link do Litterbox: %s", finalMsg, litterboxURL)
s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &msg})
} else {
fileReader, _ := os.Open(zipFilename)
defer fileReader.Close()
s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
Content: &finalMsg,
Files: []*discordgo.File{
{
Name:        filepath.Base(zipFilename),
ContentType: "application/zip",
Reader:      fileReader,
},
},
})
}
os.Remove(zipFilename)
}
for _, file := range downloadedFiles {
os.Remove(file)
}
}
}
if i.Type == discordgo.InteractionMessageComponent {
customID := i.MessageComponentData().CustomID
parts := strings.Split(customID, "_")
if strings.HasPrefix(customID, "fmt_") && len(parts) >= 4 {
mediaType := parts[1]
fmtType := parts[2]
uuidStr := parts[3]
if val, ok := activeViews.Load(uuidStr); ok {
view := val.(*ViewState)
if mediaType == "audio" {
view.AudioFmt = fmtType
} else if mediaType == "video" {
view.VideoFmt = fmtType
}
components := getFormatButtons(uuidStr, view.AudioFmt, view.VideoFmt, true, true)
if !strings.Contains(i.Message.Content, "Vídeo") && strings.Contains(i.Message.Content, "Áudio") {
components = getFormatButtons(uuidStr, view.AudioFmt, view.VideoFmt, true, false)
}
s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
Type: discordgo.InteractionResponseUpdateMessage,
Data: &discordgo.InteractionResponseData{
Components: components,
},
})
}
} else if strings.HasPrefix(customID, "confirm_") && len(parts) >= 2 {
uuidStr := parts[1]
if val, ok := activeViews.Load(uuidStr); ok {
view := val.(*ViewState)
view.Confirmed = true
s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
Type: discordgo.InteractionResponseUpdateMessage,
Data: &discordgo.InteractionResponseData{
Content:    "Processando conversão (FFmpeg)...",
Components: []discordgo.MessageComponent{},
},
})
close(view.Done)
}
}
}
}
func main() {
fallbackGames = loadFallbackGames()
dg, err := discordgo.New("Bot " + discordToken)
if err != nil {
fmt.Println("Erro ao criar sessão do Discord:", err)
return
}
dg.AddHandler(handleInteractionCreate)
err = dg.Open()
if err != nil {
fmt.Println("Erro ao abrir conexao com Discord:", err)
return
}
commands := []*discordgo.ApplicationCommand{
{
Name:        "asset",
Description: "Baixa um unico asset do Roblox de forma segura",
Options: []*discordgo.ApplicationCommandOption{
{
Type:        discordgo.ApplicationCommandOptionString,
Name:        "asset_id",
Description: "ID do asset",
Required:    true,
},
},
},
{
Name:        "assetbatch",
Description: "Baixa multiplos assets e retorna um arquivo ZIP limpo",
Options: []*discordgo.ApplicationCommandOption{
{
Type:        discordgo.ApplicationCommandOptionString,
Name:        "asset_ids",
Description: "IDs dos assets separados por virgula",
Required:    true,
},
},
},
}
for _, cmd := range commands {
_, err := dg.ApplicationCommandCreate(dg.State.User.ID, "", cmd)
if err != nil {
fmt.Printf("Não foi possivel criar o comando '%s': %v\n", cmd.Name, err)
}
}
fmt.Println("Bot esta rodando. Pressione CTRL+C para sair.")
sc := make(chan os.Signal, 1)
signal.Notify(sc, syscall.SIGINT, syscall.SIGTERM, os.Interrupt)
<-sc
dg.Close()
}