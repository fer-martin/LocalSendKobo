// localsend-recv: receptor LocalSend v2 para Kobo, con UI de control.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"html"
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

const koboVersionFile = "/mnt/onboard/.kobo/version"

// Version se inyecta en build-time vía -ldflags.
var Version = "dev"
var showVersion = flag.Bool("version", false, "print version and exit")

var (
	alias       = flag.String("alias", "", "nombre visible en LocalSend (default: auto-detected desde /mnt/onboard/.kobo/version)")
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

// detectAlias arma un alias tipo "Kobo Aura 1234" leyendo .kobo/version.
// Fuera de un Kobo, cae al hostname.
func detectAlias() string {
	serial, model := readKoboVersion(koboVersionFile)
	suffix := ""
	if len(serial) >= 4 {
		suffix = " " + serial[len(serial)-4:]
	}
	if model != "" {
		return strings.TrimSpace(model + suffix)
	}
	// No es un Kobo (o no se pudo parsear): usar hostname.
	if h, err := os.Hostname(); err == nil && h != "" {
		return h + suffix
	}
	return "LocalSend" + suffix
}

// localIP devuelve la IP IPv4 que el SO usaría para tráfico de LAN saliente.
// No envía paquetes; sólo dispara la decisión de routing. Vacía si no hay red.
func localIP() string {
	conn, err := net.Dial("udp", "192.0.2.1:1") // TEST-NET-1, no enruta a ningún lado
	if err != nil {
		return ""
	}
	defer conn.Close()
	if addr, ok := conn.LocalAddr().(*net.UDPAddr); ok {
		return addr.IP.String()
	}
	return ""
}

// Códigos de modelo Kobo (últimos 3 hex del UUID en .kobo/version).
// Fuente: KOReader / mobileread. Si tu modelo no está, agregalo y mandá PR :)
var koboModels = map[string]string{
	"310": "Kobo Touch A",
	"320": "Kobo Touch B",
	"330": "Kobo Touch C",
	"340": "Kobo Mini",
	"350": "Kobo Glo",
	"360": "Kobo Aura HD",
	"370": "Kobo Aura",
	"371": "Kobo Aura H2O",
	"372": "Kobo Aura H2O Edition 2 v1",
	"373": "Kobo Aura H2O Edition 2 v2",
	"374": "Kobo Aura Edition 2 v1",
	"375": "Kobo Aura Edition 2 v2",
	"376": "Kobo Glo HD",
	"377": "Kobo Touch 2.0",
	"378": "Kobo Aura One",
	"379": "Kobo Aura One Limited Edition",
	"37b": "Kobo Aura SE",
	"381": "Kobo Clara HD",
	"382": "Kobo Forma",
	"384": "Kobo Forma 32GB",
	"386": "Kobo Aura H2O Edition 2 v2",
	"387": "Kobo Aura SE",
	"388": "Kobo Nia",
	"389": "Kobo Elipsa",
	"38b": "Kobo Libra H2O",
	"393": "Kobo Sage",
	"394": "Kobo Libra 2",
	"396": "Kobo Elipsa 2E",
	"397": "Kobo Clara 2E",
	"398": "Kobo Clara BW",
	"399": "Kobo Clara Colour",
	"39a": "Kobo Libra Colour",
}

// readKoboVersion parsea /mnt/onboard/.kobo/version y devuelve (serial, model).
// Tolerante a las dos variantes conocidas:
//   - SERIAL,FW,TIMESTAMP,DEVICE_UUID                       (más viejo)
//   - SERIAL,FW,TIMESTAMP,DEVICE_UUID,MODEL_NAME            (más nuevo)
func readKoboVersion(path string) (serial, model string) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", ""
	}
	parts := strings.Split(strings.TrimSpace(string(b)), ",")
	for i, p := range parts {
		parts[i] = strings.TrimSpace(p)
	}
	if len(parts) > 0 {
		serial = parts[0]
	}

	// 1) ¿Hay un campo que sea texto "kobo …"?
	for _, p := range parts[1:] {
		if strings.HasPrefix(strings.ToLower(p), "kobo") {
			return serial, prettyModel(p)
		}
	}

	// 2) Si no, buscar el campo UUID y mapear los últimos 3 hex.
	for _, p := range parts[1:] {
		if looksLikeDeviceUUID(p) {
			code := strings.ToLower(p[len(p)-3:])
			if name, ok := koboModels[code]; ok {
				return serial, name
			}
			// código desconocido: devolver el código en sí, mejor que nada
			return serial, "Kobo " + strings.ToUpper(code)
		}
	}
	return serial, ""
}

func looksLikeDeviceUUID(s string) bool {
	// Aceptamos cualquier cadena con suficientes dígitos hex al final;
	// los firmwares varían en cantidad de ceros/guiones.
	if len(s) < 3 {
		return false
	}
	for _, r := range s[len(s)-3:] {
		if !(r >= '0' && r <= '9') && !(r >= 'a' && r <= 'f') && !(r >= 'A' && r <= 'F') {
			return false
		}
	}
	// y al menos un dígito hex más antes
	return strings.ContainsAny(s, "-0")
}

// prettyModel pasa "kobo libra h2o" a "Kobo Libra H2O" (sólo para el camino 1).
func prettyModel(s string) string {
	parts := strings.Fields(s)
	for i, p := range parts {
		switch strings.ToLower(p) {
		case "h2o", "hd", "2e", "se", "bw":
			parts[i] = strings.ToUpper(p)
		default:
			if len(p) > 0 {
				parts[i] = strings.ToUpper(p[:1]) + strings.ToLower(p[1:])
			}
		}
	}
	return strings.Join(parts, " ")
}

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

// showControlDialog: diálogo modal con un solo botón "Detener".
// Bloquea hasta que el usuario lo toca o hasta que dismissDialog() lo cierra.
func showControlDialog() error {
	ip := localIP()
	if ip == "" {
		ip = "(sin red)"
	}
	// Nickel renderiza el body como rich-text Qt: usar <br>, no \n.
	body := fmt.Sprintf(
		"Receptor LocalSend activo.<br><br>"+
			"• Alias: <b>%s</b><br>"+
			"• IP: <b>%s</b><br>"+
			"• Puerto: <b>%d</b><br>"+
			"• Destino: %s<br><br>"+
			"Toca «Detener» para cerrar el servidor.",
		html.EscapeString(*alias),
		html.EscapeString(ip),
		lsPort,
		html.EscapeString(*downloadDir),
	)
	cmd := exec.Command("qndb",
		"-t", "86400000", // 24 h
		"-s", "dlgConfirmResult",
		"-m", "dlgConfirmAccept", "LocalSend", body, "Detener",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("qndb dlg: %w (out=%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// dismissDialog cierra el diálogo desde fuera (si lo hay).
func dismissDialog() {
	if !haveQndb() {
		return
	}
	_ = exec.Command("qndb", "-m", "dlgConfirmClose").Run()
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

// ---------- Funciones de Wi-Fi ----------

// forceWifi activa o desactiva la persistencia del Wi-Fi en Nickel.
// Mientras esté en true, Nickel no suspende la radio.
func forceWifi(on bool) {
	if !haveQndb() {
		return
	}
	arg := "false"
	if on {
		arg = "true"
	}
	if err := exec.Command("qndb", "-m", "nsForceWifi", arg).Run(); err != nil {
		log.Printf("[wifi] nsForceWifi %s: %v", arg, err)
	} else {
		log.Printf("[wifi] nsForceWifi %s OK", arg)
	}
}

// reconnectWifi pide a Nickel que se conecte sin abrir el diálogo.
// Útil como red de seguridad si la radio cayó por otro motivo.
func reconnectWifi() {
	if !haveQndb() {
		return
	}
	_ = exec.Command("qndb", "-m", "wfmConnectWirelessSilently").Run()
}

// ---------- main ----------

func main() {
	flag.Parse()
	if *showVersion {
		fmt.Println(Version)
		os.Exit(0)
	}	
	if *alias == "" {
    	*alias = detectAlias()
	}
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
	forceWifi(true)
	defer forceWifi(false) // desactiva al salir del main

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