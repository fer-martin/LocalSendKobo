// localsend-recv: receptor LocalSend v2 para Kobo.
// - Solo recibe, auto-acepta, sin PIN ni TLS.
// - Muestra un único toast por sesión al completarse.
// - Dispara rescan de biblioteca si se recibieron libros.
package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	mcastGroup = "224.0.0.167"
	lsPort     = 53317
)

var (
	alias       = flag.String("alias", "Kobo Aura", "nombre visible en LocalSend")
	downloadDir = flag.String("dir", "/mnt/onboard/LocalSend", "carpeta destino")
	noRescan    = flag.Bool("no-rescan", false, "no pedir rescan de biblioteca tras recibir libros")
	fingerprint = randHex(16)
)

// Extensiones que disparan rescan en Nickel
var bookExts = map[string]bool{
	".epub": true, ".kepub": true, ".pdf": true,
	".cbz": true, ".cbr": true, ".txt": true,
	".mobi": true, ".html": true, ".rtf": true,
}

// ---------- Tipos del protocolo ----------

type DeviceInfo struct {
	Alias       string `json:"alias"`
	Version     string `json:"version"`
	DeviceModel string `json:"deviceModel"`
	DeviceType  string `json:"deviceType"`
	Fingerprint string `json:"fingerprint"`
	Port        int    `json:"port"`
	Protocol    string `json:"protocol"`
	Download    bool   `json:"download"`
	Announce    bool   `json:"announce,omitempty"`
}

func info(announce bool) DeviceInfo {
	return DeviceInfo{
		Alias: *alias, Version: "2.0",
		DeviceModel: "Kobo", DeviceType: "desktop",
		Fingerprint: fingerprint, Port: lsPort,
		Protocol: "http", Download: false,
		Announce: announce,
	}
}

type fileMeta struct {
	ID       string `json:"id"`
	FileName string `json:"fileName"`
	Size     int64  `json:"size"`
	FileType string `json:"fileType"`
	SHA256   string `json:"sha256,omitempty"`
	Preview  string `json:"preview,omitempty"`
}

type prepareReq struct {
	Info  json.RawMessage     `json:"info"`
	Files map[string]fileMeta `json:"files"`
}

// ---------- Estado de sesiones ----------

type fileEntry struct {
	Token    string
	FileName string
	Size     int64
	Done     bool
}

type session struct {
	senderAlias string
	files       map[string]*fileEntry
	saved       []string // rutas ya guardadas
}

var (
	mu       sync.Mutex
	sessions = map[string]*session{}
)

// ---------- Helpers ----------

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func uniquePath(p string) string {
	if _, err := os.Stat(p); os.IsNotExist(err) {
		return p
	}
	ext := filepath.Ext(p)
	base := strings.TrimSuffix(p, ext)
	for i := 1; ; i++ {
		c := fmt.Sprintf("%s_%d%s", base, i, ext)
		if _, err := os.Stat(c); os.IsNotExist(err) {
			return c
		}
	}
}

func safeName(n string) string {
	n = filepath.Base(n)
	n = strings.ReplaceAll(n, "/", "_")
	n = strings.ReplaceAll(n, "\\", "_")
	if n == "" || n == "." || n == ".." {
		n = "file_" + randHex(4)
	}
	return n
}

func hasBook(paths []string) bool {
	for _, p := range paths {
		ext := strings.ToLower(filepath.Ext(p))
		if bookExts[ext] {
			return true
		}
	}
	return false
}

// ---------- Notificaciones ----------

// notify: toast nativo vía NickelDBus; fallback a FBInk.
func notify(title, msg string) {
	if _, err := exec.LookPath("qndb"); err == nil {
		_ = exec.Command("qndb", "-m", "mwcToast", "3000", title, msg).Run()
		return
	}
	if _, err := exec.LookPath("fbink"); err == nil {
		full := fmt.Sprintf("%s: %s", title, msg)
		_ = exec.Command("fbink", "-q", "-y", "-3", "-m", "-p", full).Run()
		go func() {
			time.Sleep(3 * time.Second)
			_ = exec.Command("fbink", "-q", "-y", "-3", "-p", " ").Run()
		}()
	}
}

func rescanLibrary() {
	if *noRescan {
		return
	}
	if _, err := exec.LookPath("qndb"); err != nil {
		return
	}
	if err := exec.Command("qndb", "-m", "pfmRescanBooks").Run(); err != nil {
		log.Printf("[LS] rescan falló: %v", err)
	}
}

// ---------- Handlers HTTP ----------

func handleInfo(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, info(false))
}

func handleRegister(w http.ResponseWriter, r *http.Request) {
	_, _ = io.Copy(io.Discard, r.Body)
	writeJSON(w, 200, info(false))
}

func handlePrepare(w http.ResponseWriter, r *http.Request) {
	var req prepareReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}

	// Extraer alias del emisor (best-effort)
	senderAlias := "?"
	var senderInfo DeviceInfo
	if json.Unmarshal(req.Info, &senderInfo) == nil && senderInfo.Alias != "" {
		senderAlias = senderInfo.Alias
	}

	sid := randHex(16)
	tokens := map[string]string{}
	fmap := map[string]*fileEntry{}
	for fid, f := range req.Files {
		tok := randHex(16)
		tokens[fid] = tok
		fmap[fid] = &fileEntry{Token: tok, FileName: f.FileName, Size: f.Size}
	}
	mu.Lock()
	sessions[sid] = &session{senderAlias: senderAlias, files: fmap}
	mu.Unlock()

	log.Printf("[LS] prepare: %d archivo(s) desde %q", len(req.Files), senderAlias)
	writeJSON(w, http.StatusOK, map[string]any{
		"sessionId": sid,
		"files":     tokens,
	})
}

func handleUpload(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	sid, fid, tok := q.Get("sessionId"), q.Get("fileId"), q.Get("token")

	mu.Lock()
	ss, ok := sessions[sid]
	mu.Unlock()
	if !ok {
		http.Error(w, "no session", http.StatusForbidden)
		return
	}
	e, ok := ss.files[fid]
	if !ok || e.Token != tok {
		http.Error(w, "bad token", http.StatusForbidden)
		return
	}

	out := uniquePath(filepath.Join(*downloadDir, safeName(e.FileName)))
	f, err := os.Create(out)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	n, copyErr := io.Copy(f, r.Body)
	closeErr := f.Close()
	if copyErr != nil {
		log.Printf("[LS] err copiando %s: %v", out, copyErr)
		_ = os.Remove(out)
		http.Error(w, copyErr.Error(), http.StatusInternalServerError)
		return
	}
	if closeErr != nil {
		log.Printf("[LS] err cerrando %s: %v", out, closeErr)
	}
	log.Printf("[LS] guardado: %s (%d B)", out, n)

	// Marcar completo y ver si la sesión terminó
	var done bool
	var saved []string
	var sender string
	mu.Lock()
	e.Done = true
	ss.saved = append(ss.saved, out)
	done = true
	for _, fe := range ss.files {
		if !fe.Done {
			done = false
			break
		}
	}
	if done {
		saved = append(saved, ss.saved...)
		sender = ss.senderAlias
		delete(sessions, sid)
	}
	mu.Unlock()

	w.WriteHeader(http.StatusOK)

	if done {
		// Notificar fuera del hot path HTTP
		go func() {
			var msg string
			if len(saved) == 1 {
				msg = "Recibido: " + filepath.Base(saved[0])
			} else {
				msg = fmt.Sprintf("Recibidos %d archivos de %s", len(saved), sender)
			}
			notify("LocalSend", msg)
			if hasBook(saved) {
				rescanLibrary()
			}
		}()
	}
}

func handleCancel(w http.ResponseWriter, r *http.Request) {
	sid := r.URL.Query().Get("sessionId")
	mu.Lock()
	delete(sessions, sid)
	mu.Unlock()
	w.WriteHeader(http.StatusOK)
}

// ---------- Descubrimiento UDP ----------

func announcer() {
	addr := &net.UDPAddr{IP: net.ParseIP(mcastGroup), Port: lsPort}
	c, err := net.DialUDP("udp4", nil, addr)
	if err != nil {
		log.Printf("[UDP tx] %v", err)
		return
	}
	defer c.Close()
	msg, _ := json.Marshal(info(true))
	for {
		if _, err := c.Write(msg); err != nil {
			log.Printf("[UDP tx] %v", err)
		}
		time.Sleep(5 * time.Second)
	}
}

func listener() {
	addr := &net.UDPAddr{IP: net.ParseIP(mcastGroup), Port: lsPort}
	c, err := net.ListenMulticastUDP("udp4", nil, addr)
	if err != nil {
		log.Printf("[UDP rx] %v", err)
		return
	}
	_ = c.SetReadBuffer(65536)
	reply, _ := json.Marshal(info(false))
	buf := make([]byte, 8192)
	for {
		n, src, err := c.ReadFromUDP(buf)
		if err != nil {
			log.Printf("[UDP rx] %v", err)
			continue
		}
		var m DeviceInfo
		if json.Unmarshal(buf[:n], &m) != nil {
			continue
		}
		if m.Fingerprint == fingerprint || !m.Announce {
			continue
		}
		dst := &net.UDPAddr{IP: src.IP, Port: lsPort}
		if uc, err := net.DialUDP("udp4", nil, dst); err == nil {
			_, _ = uc.Write(reply)
			_ = uc.Close()
		}
	}
}

// ---------- main ----------

func main() {
	flag.Parse()
	if err := os.MkdirAll(*downloadDir, 0o755); err != nil {
		log.Fatalf("no puedo crear %s: %v", *downloadDir, err)
	}
	log.Printf("[LS] alias=%q fp=%s dir=%s", *alias, fingerprint, *downloadDir)

	mux := http.NewServeMux()
	mux.HandleFunc("/api/localsend/v2/info", handleInfo)
	mux.HandleFunc("/api/localsend/v2/register", handleRegister)
	mux.HandleFunc("/api/localsend/v2/prepare-upload", handlePrepare)
	mux.HandleFunc("/api/localsend/v2/upload", handleUpload)
	mux.HandleFunc("/api/localsend/v2/cancel", handleCancel)

	go listener()
	go announcer()

	addr := fmt.Sprintf(":%d", lsPort)
	log.Printf("[LS] HTTP en %s", addr)
	srv := &http.Server{
		Addr:         addr,
		Handler:      mux,
		ReadTimeout:  0, // sin límite: archivos grandes
		WriteTimeout: 0,
	}
	log.Fatal(srv.ListenAndServe())
}