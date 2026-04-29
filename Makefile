# ---- Configuración ------------------------------------------------------
BINARY      := localsend-recv
PKG         := .

# Kobo (ajustá a tu IP)
KOBO_HOST   ?= 192.168.1.50
KOBO_USER   ?= root
KOBO_DIR    ?= /mnt/onboard/.adds
SSH_OPTS    ?= -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null

# Local (para probar en PC)
LOCAL_DIR   ?= ./downloads
LOCAL_ALIAS ?= Dev PC

# Flags de compilación
LDFLAGS     := -s -w
GOFLAGS     := -trimpath

# Cross-compile target (Kobo Aura = ARMv7)
KOBO_ENV    := CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7

# ---- Targets ------------------------------------------------------------

.PHONY: help
help: ## Muestra esta ayuda
	@awk 'BEGIN{FS=":.*##"; printf "\nTargets:\n"} /^[a-zA-Z_-]+:.*##/ {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

.PHONY: build
build: ## Compila para tu máquina (debug local)
	go build $(GOFLAGS) -ldflags="$(LDFLAGS)" -o $(BINARY) $(PKG)

.PHONY: kobo
kobo: ## Compila binario estático ARMv7 para Kobo
	$(KOBO_ENV) go build $(GOFLAGS) -ldflags="$(LDFLAGS)" -o $(BINARY) $(PKG)
	@file $(BINARY)
	@ls -lh $(BINARY)

.PHONY: run
run: build ## Corre localmente (usa LOCAL_DIR / LOCAL_ALIAS)
	mkdir -p $(LOCAL_DIR)
	./$(BINARY) -dir $(LOCAL_DIR) -alias "$(LOCAL_ALIAS)" -no-ui

.PHONY: deploy
deploy: kobo ## scp del binario al Kobo
	ssh $(KOBO_HOST) "mkdir -p $(KOBO_DIR)"
	scp $(BINARY) $(KOBO_HOST):$(KOBO_DIR)/$(BINARY)
	ssh $(KOBO_HOST) "chmod +x $(KOBO_DIR)/$(BINARY)"
	@echo "OK: desplegado en $(KOBO_HOST):$(KOBO_DIR)/$(BINARY)"

.PHONY: deploy-nm
deploy-nm: ## Copia entradas de NickelMenu (start/stop) al Kobo
	@printf 'menu_item:main:LocalSend (start):cmd_spawn:quiet:exec %s/%s\nmenu_item:main:LocalSend (stop) :cmd_spawn:quiet:exec killall -TERM %s\n' \
		"$(KOBO_DIR)" "$(BINARY)" "$(BINARY)" > .nm-localsend.tmp
	scp .nm-localsend.tmp $(KOBO_HOST):/mnt/onboard/.adds/nm/localsend
	rm -f .nm-localsend.tmp
	@echo "OK: NickelMenu actualizado (reiniciá Nickel para recargar el menú)"

.PHONY: start
start: ## Lanza el receptor en el Kobo (en background)
	ssh $(KOBO_HOST) "$(KOBO_DIR)/$(BINARY) >/tmp/localsend.log 2>&1 &"
	@echo "lanzado; logs en el Kobo: /tmp/localsend.log"

.PHONY: stop
stop: ## Detiene el receptor en el Kobo (SIGTERM)
	-ssh $(KOBO_HOST) "killall -TERM $(BINARY)"

.PHONY: log
log: ## Sigue el log del Kobo
	ssh $(KOBO_HOST) "tail -F /tmp/localsend.log"

.PHONY: ssh
ssh: ## Abre sesión SSH en el Kobo
	ssh $(KOBO_HOST)

.PHONY: ping
ping: ## Verifica que el Kobo responde
	@ssh $(SSH_OPTS) -o ConnectTimeout=3 $(KOBO_USER)@$(KOBO_HOST) "uname -a; uptime" \
		|| { echo "no se pudo conectar a $(KOBO_HOST)"; exit 1; }

.PHONY: tidy
tidy: ## go mod tidy
	go mod tidy

.PHONY: vet
vet: ## go vet
	go vet ./...

.PHONY: fmt
fmt: ## gofmt
	gofmt -w .

.PHONY: clean
clean: ## Borra binarios locales
	rm -f $(BINARY)