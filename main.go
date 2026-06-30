package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"embed"
	"encoding/base64"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"
)

//go:embed web/index.html
var webFS embed.FS

// ---------- модели данных (свой простой JSON-формат) ----------

type Auth struct {
	Type     string `json:"type"` // none | basic | bearer
	Username string `json:"username"`
	Password string `json:"password"`
	Token    string `json:"token"`
}

type Request struct {
	Name     string            `json:"name"`
	Method   string            `json:"method"`
	URL      string            `json:"url"`
	Headers  map[string]string `json:"headers"`
	Auth     Auth              `json:"auth"`
	Body     string            `json:"body"`
	Insecure bool              `json:"insecure"` // не проверять TLS-сертификат
}

type Collection struct {
	Name     string    `json:"name"`
	Requests []Request `json:"requests"`
}

type SendResult struct {
	OK         bool              `json:"ok"`
	Error      string            `json:"error,omitempty"`
	Status     int               `json:"status"`
	StatusText string            `json:"statusText"`
	Headers    map[string]string `json:"headers"`
	Body       string            `json:"body"`
	TimeMs     int64             `json:"timeMs"`
	Size       int               `json:"size"`
	URL        string            `json:"url"`
}

// ---------- путь к коллекциям (рядом с исполняемым файлом) ----------

func collectionsDir() string {
	exe, err := os.Executable()
	base := "."
	if err == nil {
		base = filepath.Dir(exe)
	}
	dir := filepath.Join(base, "collections")
	_ = os.MkdirAll(dir, 0o755)
	return dir
}

// ---------- подстановка плейсхолдеров {{col}} ----------

var placeholderRe = regexp.MustCompile(`\{\{\s*([^}]+?)\s*\}\}`)

func substitute(s string, row map[string]string) string {
	return placeholderRe.ReplaceAllStringFunc(s, func(m string) string {
		key := strings.TrimSpace(placeholderRe.FindStringSubmatch(m)[1])
		if v, ok := row[key]; ok {
			return v
		}
		return m // оставляем как есть, если колонки нет
	})
}

func applyRow(r Request, row map[string]string) Request {
	out := r
	out.URL = substitute(r.URL, row)
	out.Body = substitute(r.Body, row)
	out.Headers = map[string]string{}
	for k, v := range r.Headers {
		out.Headers[substitute(k, row)] = substitute(v, row)
	}
	out.Auth = Auth{
		Type:     r.Auth.Type,
		Username: substitute(r.Auth.Username, row),
		Password: substitute(r.Auth.Password, row),
		Token:    substitute(r.Auth.Token, row),
	}
	return out
}

// ---------- выполнение одного запроса ----------

func doSend(ctx context.Context, r Request) SendResult {
	start := time.Now()
	method := strings.ToUpper(strings.TrimSpace(r.Method))
	if method == "" {
		method = "GET"
	}

	var bodyReader io.Reader
	if r.Body != "" && method != "GET" && method != "HEAD" {
		bodyReader = bytes.NewBufferString(r.Body)
	}

	req, err := http.NewRequestWithContext(ctx, method, strings.TrimSpace(r.URL), bodyReader)
	if err != nil {
		return SendResult{OK: false, Error: "Неверный запрос: " + err.Error(), URL: r.URL}
	}

	for k, v := range r.Headers {
		if strings.TrimSpace(k) == "" {
			continue
		}
		req.Header.Set(k, v)
	}

	// авторизация
	switch r.Auth.Type {
	case "basic":
		token := base64.StdEncoding.EncodeToString([]byte(r.Auth.Username + ":" + r.Auth.Password))
		req.Header.Set("Authorization", "Basic "+token)
	case "bearer":
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(r.Auth.Token))
	}

	// тело по умолчанию JSON, если не задан Content-Type
	if bodyReader != nil && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}

	transport := &http.Transport{}
	if r.Insecure {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}
	client := &http.Client{Timeout: 120 * time.Second, Transport: transport}

	resp, err := client.Do(req)
	if err != nil {
		return SendResult{OK: false, Error: "Ошибка сети: " + err.Error(), TimeMs: time.Since(start).Milliseconds(), URL: r.URL}
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	headers := map[string]string{}
	for k, v := range resp.Header {
		headers[k] = strings.Join(v, ", ")
	}

	return SendResult{
		OK:         true,
		Status:     resp.StatusCode,
		StatusText: resp.Status,
		Headers:    headers,
		Body:       string(raw),
		TimeMs:     time.Since(start).Milliseconds(),
		Size:       len(raw),
		URL:        r.URL,
	}
}

// ---------- HTTP-ручки ----------

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func handleSend(w http.ResponseWriter, r *http.Request) {
	var req Request
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, map[string]string{"error": "Плохой JSON: " + err.Error()})
		return
	}
	writeJSON(w, 200, doSend(r.Context(), req))
}

type batchReq struct {
	Request   Request `json:"request"`
	CSV       string  `json:"csv"`
	Delimiter string  `json:"delimiter"` // один символ, по умолчанию ","
	DelayMs   int     `json:"delayMs"`   // пауза между запросами, мс
}

func parseCSV(content, delim string) ([]map[string]string, error) {
	rd := csv.NewReader(strings.NewReader(content))
	if delim != "" {
		rd.Comma = []rune(delim)[0]
	}
	rd.TrimLeadingSpace = true
	rd.FieldsPerRecord = -1 // допускаем разное число полей
	records, err := rd.ReadAll()
	if err != nil {
		return nil, err
	}
	if len(records) == 0 {
		return nil, fmt.Errorf("пустой CSV")
	}
	header := records[0]
	var rows []map[string]string
	for _, rec := range records[1:] {
		m := map[string]string{}
		for i, h := range header {
			if i < len(rec) {
				m[strings.TrimSpace(h)] = rec[i]
			} else {
				m[strings.TrimSpace(h)] = ""
			}
		}
		rows = append(rows, m)
	}
	return rows, nil
}

// handleBatch выполняет прогон по CSV и отдаёт результаты потоком (NDJSON):
// сначала строка {"type":"meta","total":N}, затем по строке на каждый запрос
// {"type":"row","index":i,"row":{...},"result":{...}}. Клиент читает поток в
// реальном времени; отмена через AbortController обрывает контекст запроса —
// цикл это замечает и останавливается.
func handleBatch(w http.ResponseWriter, r *http.Request) {
	var br batchReq
	if err := json.NewDecoder(r.Body).Decode(&br); err != nil {
		writeJSON(w, 400, map[string]string{"error": "Плохой JSON: " + err.Error()})
		return
	}
	rows, err := parseCSV(br.CSV, br.Delimiter)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": "Ошибка CSV: " + err.Error()})
		return
	}

	w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	flusher, _ := w.(http.Flusher)
	enc := json.NewEncoder(w)
	ctx := r.Context()

	_ = enc.Encode(map[string]any{"type": "meta", "total": len(rows)})
	if flusher != nil {
		flusher.Flush()
	}

	for idx, row := range rows {
		if ctx.Err() != nil { // клиент остановил прогон
			break
		}
		if idx > 0 && br.DelayMs > 0 { // пауза между запросами (с учётом отмены)
			select {
			case <-time.After(time.Duration(br.DelayMs) * time.Millisecond):
			case <-ctx.Done():
			}
			if ctx.Err() != nil {
				break
			}
		}
		req := applyRow(br.Request, row)
		res := doSend(ctx, req)
		if ctx.Err() != nil { // прервано во время запроса — не шлём ложную ошибку
			break
		}
		_ = enc.Encode(map[string]any{"type": "row", "index": idx, "row": row, "result": res})
		if flusher != nil {
			flusher.Flush()
		}
	}
}

func handleListCollections(w http.ResponseWriter, r *http.Request) {
	dir := collectionsDir()
	entries, _ := os.ReadDir(dir)
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(strings.ToLower(e.Name()), ".json") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	writeJSON(w, 200, names)
}

func handleLoadCollection(w http.ResponseWriter, r *http.Request) {
	name := filepath.Base(r.URL.Query().Get("file"))
	if name == "" || name == "." {
		writeJSON(w, 400, map[string]string{"error": "не указан файл"})
		return
	}
	data, err := os.ReadFile(filepath.Join(collectionsDir(), name))
	if err != nil {
		writeJSON(w, 404, map[string]string{"error": err.Error()})
		return
	}
	var col Collection
	if err := json.Unmarshal(data, &col); err != nil {
		writeJSON(w, 400, map[string]string{"error": "файл повреждён: " + err.Error()})
		return
	}
	writeJSON(w, 200, col)
}

type saveReq struct {
	File       string  `json:"file"`       // имя файла, напр. "my.json"
	Append     bool    `json:"append"`     // дописать в существующую коллекцию
	Collection *Collection `json:"collection"` // при создании/перезаписи
	Request    *Request    `json:"request"`    // при append одного запроса
}

func handleSaveCollection(w http.ResponseWriter, r *http.Request) {
	var sr saveReq
	if err := json.NewDecoder(r.Body).Decode(&sr); err != nil {
		writeJSON(w, 400, map[string]string{"error": "Плохой JSON: " + err.Error()})
		return
	}
	name := filepath.Base(strings.TrimSpace(sr.File))
	if name == "" || name == "." {
		writeJSON(w, 400, map[string]string{"error": "не указано имя файла"})
		return
	}
	if !strings.HasSuffix(strings.ToLower(name), ".json") {
		name += ".json"
	}
	path := filepath.Join(collectionsDir(), name)

	var col Collection
	if sr.Append {
		// читаем существующую (если есть) и дописываем запрос
		if data, err := os.ReadFile(path); err == nil {
			_ = json.Unmarshal(data, &col)
		}
		if col.Name == "" {
			col.Name = strings.TrimSuffix(name, ".json")
		}
		if sr.Request != nil {
			col.Requests = append(col.Requests, *sr.Request)
		}
	} else if sr.Collection != nil {
		col = *sr.Collection
		if col.Name == "" {
			col.Name = strings.TrimSuffix(name, ".json")
		}
	} else {
		writeJSON(w, 400, map[string]string{"error": "нечего сохранять"})
		return
	}

	data, _ := json.MarshalIndent(col, "", "  ")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "file": name, "count": len(col.Requests)})
}

type deleteReq struct {
	File  string `json:"file"`
	Index int    `json:"index"`
}

func handleDeleteRequest(w http.ResponseWriter, r *http.Request) {
	var dr deleteReq
	if err := json.NewDecoder(r.Body).Decode(&dr); err != nil {
		writeJSON(w, 400, map[string]string{"error": "Плохой JSON: " + err.Error()})
		return
	}
	name := filepath.Base(strings.TrimSpace(dr.File))
	if name == "" || name == "." {
		writeJSON(w, 400, map[string]string{"error": "не указано имя файла"})
		return
	}
	if !strings.HasSuffix(strings.ToLower(name), ".json") {
		name += ".json"
	}
	path := filepath.Join(collectionsDir(), name)

	data, err := os.ReadFile(path)
	if err != nil {
		writeJSON(w, 404, map[string]string{"error": err.Error()})
		return
	}
	var col Collection
	if err := json.Unmarshal(data, &col); err != nil {
		writeJSON(w, 400, map[string]string{"error": "файл повреждён: " + err.Error()})
		return
	}
	if dr.Index < 0 || dr.Index >= len(col.Requests) {
		writeJSON(w, 400, map[string]string{"error": "неверный индекс запроса"})
		return
	}
	col.Requests = append(col.Requests[:dr.Index], col.Requests[dr.Index+1:]...)

	out, _ := json.MarshalIndent(col, "", "  ")
	if err := os.WriteFile(path, out, 0o644); err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "file": name, "count": len(col.Requests)})
}

// loadCol читает и парсит коллекцию по имени файла (с нормализацией имени).
func loadCol(file string) (Collection, string, int, error) {
	name := filepath.Base(strings.TrimSpace(file))
	if name == "" || name == "." {
		return Collection{}, "", 400, fmt.Errorf("не указано имя файла")
	}
	if !strings.HasSuffix(strings.ToLower(name), ".json") {
		name += ".json"
	}
	path := filepath.Join(collectionsDir(), name)
	data, err := os.ReadFile(path)
	if err != nil {
		return Collection{}, name, 404, err
	}
	var col Collection
	if err := json.Unmarshal(data, &col); err != nil {
		return Collection{}, name, 400, fmt.Errorf("файл повреждён: %s", err.Error())
	}
	return col, name, 200, nil
}

func writeCol(name string, col Collection) error {
	data, _ := json.MarshalIndent(col, "", "  ")
	return os.WriteFile(filepath.Join(collectionsDir(), name), data, 0o644)
}

type updateReq struct {
	File    string   `json:"file"`
	Index   int      `json:"index"`
	Request *Request `json:"request"`
}

// handleUpdateRequest заменяет запрос по индексу (обновление «по месту»).
func handleUpdateRequest(w http.ResponseWriter, r *http.Request) {
	var ur updateReq
	if err := json.NewDecoder(r.Body).Decode(&ur); err != nil {
		writeJSON(w, 400, map[string]string{"error": "Плохой JSON: " + err.Error()})
		return
	}
	col, name, code, err := loadCol(ur.File)
	if err != nil {
		writeJSON(w, code, map[string]string{"error": err.Error()})
		return
	}
	if ur.Request == nil {
		writeJSON(w, 400, map[string]string{"error": "нет запроса для обновления"})
		return
	}
	if ur.Index < 0 || ur.Index >= len(col.Requests) {
		writeJSON(w, 400, map[string]string{"error": "неверный индекс запроса"})
		return
	}
	col.Requests[ur.Index] = *ur.Request
	if err := writeCol(name, col); err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "file": name, "count": len(col.Requests), "index": ur.Index})
}

// handleDuplicateRequest вставляет копию запроса сразу после оригинала.
func handleDuplicateRequest(w http.ResponseWriter, r *http.Request) {
	var dr deleteReq // {file, index}
	if err := json.NewDecoder(r.Body).Decode(&dr); err != nil {
		writeJSON(w, 400, map[string]string{"error": "Плохой JSON: " + err.Error()})
		return
	}
	col, name, code, err := loadCol(dr.File)
	if err != nil {
		writeJSON(w, code, map[string]string{"error": err.Error()})
		return
	}
	if dr.Index < 0 || dr.Index >= len(col.Requests) {
		writeJSON(w, 400, map[string]string{"error": "неверный индекс запроса"})
		return
	}
	cp := col.Requests[dr.Index]
	cp.Name = strings.TrimSpace(cp.Name) + " (копия)"
	tail := append([]Request{cp}, col.Requests[dr.Index+1:]...)
	col.Requests = append(col.Requests[:dr.Index+1], tail...)
	if err := writeCol(name, col); err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "file": name, "count": len(col.Requests), "index": dr.Index + 1})
}

type renameColReq struct {
	File    string `json:"file"`
	NewFile string `json:"newFile"`
}

// handleRenameCollection переименовывает файл коллекции и её отображаемое имя.
func handleRenameCollection(w http.ResponseWriter, r *http.Request) {
	var rr renameColReq
	if err := json.NewDecoder(r.Body).Decode(&rr); err != nil {
		writeJSON(w, 400, map[string]string{"error": "Плохой JSON: " + err.Error()})
		return
	}
	col, name, code, err := loadCol(rr.File)
	if err != nil {
		writeJSON(w, code, map[string]string{"error": err.Error()})
		return
	}
	newName := filepath.Base(strings.TrimSpace(rr.NewFile))
	if newName == "" || newName == "." {
		writeJSON(w, 400, map[string]string{"error": "не указано новое имя"})
		return
	}
	if !strings.HasSuffix(strings.ToLower(newName), ".json") {
		newName += ".json"
	}
	newPath := filepath.Join(collectionsDir(), newName)
	if newName != name {
		if _, e := os.Stat(newPath); e == nil {
			writeJSON(w, 400, map[string]string{"error": "коллекция с таким именем уже существует"})
			return
		}
	}
	col.Name = strings.TrimSuffix(newName, ".json")
	if err := writeCol(newName, col); err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	if newName != name {
		_ = os.Remove(filepath.Join(collectionsDir(), name))
	}
	writeJSON(w, 200, map[string]any{"ok": true, "file": newName, "count": len(col.Requests)})
}

func handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	data, err := webFS.ReadFile("web/index.html")
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(data)
}

func openBrowser(url string) {
	var cmd string
	var args []string
	switch runtime.GOOS {
	case "windows":
		cmd = "rundll32"
		args = []string{"url.dll,FileProtocolHandler", url}
	case "darwin":
		cmd = "open"
		args = []string{url}
	default:
		cmd = "xdg-open"
		args = []string{url}
	}
	_ = exec.Command(cmd, args...).Start()
}

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("/", handleIndex)
	mux.HandleFunc("/api/send", handleSend)
	mux.HandleFunc("/api/batch", handleBatch)
	mux.HandleFunc("/api/collections", handleListCollections)
	mux.HandleFunc("/api/collection", handleLoadCollection)
	mux.HandleFunc("/api/collection/save", handleSaveCollection)
	mux.HandleFunc("/api/collection/delete", handleDeleteRequest)
	mux.HandleFunc("/api/collection/update", handleUpdateRequest)
	mux.HandleFunc("/api/collection/duplicate", handleDuplicateRequest)
	mux.HandleFunc("/api/collection/rename", handleRenameCollection)

	// ищем свободный порт, начиная с 8787
	var ln net.Listener
	var err error
	port := 8787
	for i := 0; i < 50; i++ {
		ln, err = net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err == nil {
			break
		}
		port++
	}
	if err != nil {
		fmt.Println("Не удалось занять порт:", err)
		os.Exit(1)
	}

	url := fmt.Sprintf("http://127.0.0.1:%d", port)
	fmt.Println("PashMan запущен:", url)
	fmt.Println("Коллекции хранятся в:", collectionsDir())
	fmt.Println("Закрой это окно, чтобы остановить сервер.")
	go openBrowser(url)

	srv := &http.Server{Handler: mux}
	if err := srv.Serve(ln); err != nil {
		fmt.Println("Сервер остановлен:", err)
	}
}
