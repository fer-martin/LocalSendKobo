// localsend-recv: receptor LocalSend v2 para Kobo, con UI de control.
package main

import (
	"context"
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
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	mcastGroup = "224.0.0.167"
	lsPort     = 53317
)

var (
	alias       = flag.String("alias", "Kobo Aura", "nombre visible en LocalSend")
	downloadDir = flag.String("dir", "/mnt/onboard/LocalSend", "carpeta destino")
	noRescan    = flag.Bool("no-rescan", false, "no pedir rescan de biblioteca")
	noUI        = flag.Bool("no-ui", false, "no mostrar diálogo de control")
	fingerprint = randHex(16)
)

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

type fileEntry struct {
	Token, FileName string
	Size            int64
	Done            bool
}

type session struct {
	senderAlias string
	files       map[string]*fileEntry
	saved       []string
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
		if bookExts[strings.ToLower(filepath.Ext(p))] {
			return true
		}
	}
	return false
}

// ---------- Notificaciones / UI ----------

func haveQndb() bool {
	_, err := exec.LookPath("qndb")
	return err == nil
}

func haveFbink() bool {
	_, err := exec.LookPath("fbink")
	return err == nil
}

func notify(title, msg string) {
	if haveQndb() {
		_ = exec.Command("qndb", "-m", "mwcToast", "3000", title, msg).Run()
		return
	}
	if haveFbink() {
		full := fmt.Sprintf("%s: %s", title, msg)
		_ = exec.Command("fbink", "-q", "-y", "-3", "-m", "-p", full).Run()
		go func() {
			time.Sleep(3 * time.Second)
			_ = exec.Command("fbink", "-q", "-y", "-3", "-p", " ").Run()
		}()
	}
}

func rescanLibrary() {
	if *noRescan || !haveQndb() {
		return
	}
	if err := exec.Command("qndb", "-m", "pfmRescanBooks").Run(); err != nil {
		log.Printf("[LS] rescan falló: %v", err)
	}
}

// showControlDialog abre un diálogo modal de Nickel y bloquea hasta que el
// usuario toca "Detener" (o alguien llama a dismissDialog desde fuera).
func showControlDialog() error {
	body := fmt.Sprintf(
		"Receptor LocalSend activo.\n\n• Alias: %s\n• Puerto: %d\n• Destino: %s\n\nToca «Detener» para cerrar el servidor.",
		*alias, lsPort, *downloadDir,
	)
	cmd := exec.Command("qndb",
		"-t", "86400000", // 24 h por si acaso
		"-s", "dlgConfirmResult",
		"-m", "dlgConfirmCreate",
		"-m", "dlgConfirmSetModal", "true",
		"-m", "dlgConfirmSetTitle", "LocalSend",
		"-m", "dlgConfirmSetBody", body,
		"-m", "dlgConfirmSetAccept", "Detener",
		"-m", "dlgConfirmSetReject", "",
		"-m", "dlgConfirmShow",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("qndb dlg: %w (out=%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// dismissDialog cierra el diálogo de control si está abierto (vía señal externa).
func dismissDialog() {
	if !haveQndb() {
		return
	}
	_ = exec.Command("qndb", "-m", "dlgConfirmAccept").Run()
}

// ---------- Handlers HTTP ----------

func handleInfo(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, info(false)) }

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
	writeJSON(w, http.StatusOK, map[string]any{"sessionId": sid, "files": tokens})
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

func announcer(stop <-chan struct{}) {
	addr := &net.UDPAddr{IP: net.ParseIP(mcastGroup), Port: lsPort}
	c, err := net.DialUDP("udp4", nil, addr)
	if err != nil {
		log.Printf("[UDP tx] %v", err)
		return
	}
	defer c.Close()
	msg, _ := json.Marshal(info(true))
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	// envío inmediato
	_, _ = c.Write(msg)
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			if _, err := c.Write(msg); err != nil {
				log.Printf("[UDP tx] %v", err)
			}
		}
	}
}

func listener(stop <-chan struct{}) {
	addr := &net.UDPAddr{IP: net.ParseIP(mcastGroup), Port: lsPort}
	c, err := net.ListenMulticastUDP("udp4", nil, addr)
	if err != nil {
		log.Printf("[UDP rx] %v", err)
		return
	}
	defer c.Close()
	go func() { <-stop; _ = c.Close() }()

	_ = c.SetReadBuffer(65536)
	reply, _ := json.Marshal(info(false))
	buf := make([]byte, 8192)
	for {
		n, src, err := c.ReadFromUDP(buf)
		if err != nil {
			return
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

	// HTTP
	mux := http.NewServeMux()
	mux.HandleFunc("/api/localsend/v2/info", handleInfo)
	mux.HandleFunc("/api/localsend/v2/register", handleRegister)
	mux.HandleFunc("/api/localsend/v2/prepare-upload", handlePrepare)
	mux.HandleFunc("/api/localsend/v2/upload", handleUpload)
	mux.HandleFunc("/api/localsend/v2/cancel", handleCancel)

	srv := &http.Server{Addr: fmt.Sprintf(":%d", lsPort), Handler: mux}

	stopUDP := make(chan struct{})
	go listener(stopUDP)
	go announcer(stopUDP)

	go func() {
		log.Printf("[LS] HTTP en %s", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()

	// UI
	useUI := !*noUI && haveQndb()
	var uiDone chan struct{}
	if useUI {
		uiDone = make(chan struct{})
		go func() {
			defer close(uiDone)
			if err := showControlDialog(); err != nil {
				log.Printf("[UI] %v", err)
			}
		}()
		// pequeño "ya estoy" además del diálogo
		notify("LocalSend", fmt.Sprintf("Activo en puerto %d", lsPort))
	} else {
		log.Println("[LS] sin UI; cierra con SIGTERM (killall -TERM localsend-recv)")
		notify("LocalSend", fmt.Sprintf("Activo en puerto %d", lsPort))
	}

	// Esperar señal o cierre por usuario
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case sig := <-sigCh:
		log.Printf("[LS] señal: %v", sig)
		if useUI {
			dismissDialog()
			<-uiDone
		}
	case <-uiDone: // bloquea para siempre si useUI=false (canal nil)
		log.Println("[LS] cerrado por usuario")
	}

	// Shutdown ordenado
	close(stopUDP)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)

	notify("LocalSend", "Receptor detenido")
	log.Println("[LS] bye")
}