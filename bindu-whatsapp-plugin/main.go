package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"bindunet/sdk"
	"github.com/mdp/qrterminal/v3"
	_ "modernc.org/sqlite"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
	"rsc.io/qr"
)

type Contact struct {
	Name  string
	Phone string
	Lang  string
}

type Plugin struct {
	webhookURL       string
	waClient         *whatsmeow.Client
	mu               sync.Mutex
	lastSentPerTag   map[string]time.Time // key: phone + ":" + epcHex
	cooldown         time.Duration
	recipientsPath   string
	recipientsMTime  time.Time
	cachedRecipients []Contact
}

func NewPlugin() (*Plugin, error) {
	webhook := os.Getenv("WHATSAPP_WEBHOOK_URL")

	// Determine address book file path
	recipientsPath := os.Getenv("WHATSAPP_RECIPIENTS_FILE")
	if recipientsPath == "" {
		candidates := []string{
			"/home/dtp/.bindu/root/runtime/data/recipients.txt",
			"/home/dtp/solutions/RFID-mas-solution/bindu-whatsapp-plugin/recipients.txt",
			"recipients.txt",
		}
		for _, c := range candidates {
			if _, err := os.Stat(c); err == nil {
				recipientsPath = c
				break
			}
		}
		if recipientsPath == "" {
			recipientsPath = "/home/dtp/.bindu/root/runtime/data/recipients.txt"
		}
	}

	cooldownDuration := 10 * time.Minute
	if cdStr := os.Getenv("WHATSAPP_COOLDOWN_SEC"); cdStr != "" {
		if sec, err := strconv.Atoi(cdStr); err == nil && sec > 0 {
			cooldownDuration = time.Duration(sec) * time.Second
		}
	} else {
		// Default 30s during active engineering testing
		cooldownDuration = 30 * time.Second
	}

	p := &Plugin{
		webhookURL:     webhook,
		lastSentPerTag: make(map[string]time.Time),
		cooldown:       cooldownDuration,
		recipientsPath: recipientsPath,
	}

	// Initial load of contacts
	p.loadContacts()

	go p.initWhatsAppWeb()

	return p, nil
}

func (p *Plugin) loadContacts() []Contact {
	p.mu.Lock()
	defer p.mu.Unlock()

	stat, err := os.Stat(p.recipientsPath)
	if err != nil {
		if len(p.cachedRecipients) > 0 {
			return p.cachedRecipients
		}
		// Fallback default contacts if file not found
		return []Contact{
			{Name: "David", Phone: "+34652948185", Lang: "ES"},
			{Name: "Fabio", Phone: "+393397982711", Lang: "IT"},
			{Name: "Gonzalo", Phone: "+34606342017", Lang: "ES"},
			{Name: "Guillem", Phone: "+34620996148", Lang: "ES"},
			{Name: "Daniel", Phone: "+41799512658", Lang: "ES"},
		}
	}

	if !stat.ModTime().After(p.recipientsMTime) && len(p.cachedRecipients) > 0 {
		return p.cachedRecipients
	}

	file, err := os.Open(p.recipientsPath)
	if err != nil {
		log.Printf("[WHATSAPP] ⚠️ Error abriendo %s: %v", p.recipientsPath, err)
		return p.cachedRecipients
	}
	defer file.Close()

	var contacts []Contact
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Split(line, ":")
		if len(parts) >= 2 {
			name := strings.TrimSpace(parts[0])
			phone := strings.TrimSpace(parts[1])
			lang := ""
			if len(parts) >= 3 {
				lang = strings.ToUpper(strings.TrimSpace(parts[2]))
			}
			if lang == "" {
				lang = detectLanguage(phone, "")
			}
			contacts = append(contacts, Contact{
				Name:  name,
				Phone: phone,
				Lang:  lang,
			})
		}
	}

	p.cachedRecipients = contacts
	p.recipientsMTime = stat.ModTime()
	log.Printf("[WHATSAPP] 📖 Libreta de direcciones cargada (%d contactos) desde %s", len(contacts), p.recipientsPath)
	return contacts
}

func detectLanguage(phone, explicitLang string) string {
	if explicitLang != "" {
		return strings.ToUpper(strings.TrimSpace(explicitLang))
	}
	clean := strings.TrimPrefix(strings.TrimSpace(phone), "+")
	if strings.HasPrefix(clean, "34") {
		return "ES"
	} else if strings.HasPrefix(clean, "39") {
		return "IT"
	} else if strings.HasPrefix(clean, "33") {
		return "FR"
	} else if strings.HasPrefix(clean, "49") {
		return "DE"
	} else if strings.HasPrefix(clean, "351") {
		return "PT"
	} else if strings.HasPrefix(clean, "44") || strings.HasPrefix(clean, "1") {
		return "EN"
	}
	return "ES"
}

func (p *Plugin) initWhatsAppWeb() {
	dbDir := "/home/dtp/.bindu/root/runtime/data"
	_ = os.MkdirAll(dbDir, 0755)
	dbPath := filepath.Join(dbDir, "whatsapp_web_session.db")

	container, err := sqlstore.New(context.Background(), "sqlite", "file:"+dbPath+"?_pragma=foreign_keys(1)", waLog.Noop)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[WHATSAPP BPR] ⚠️ No se pudo inicializar almacén de sesiones: %v\n", err)
		return
	}

	deviceStore, err := container.GetFirstDevice(context.Background())
	if err != nil {
		fmt.Fprintf(os.Stderr, "[WHATSAPP BPR] ⚠️ Error obteniendo dispositivo WhatsApp: %v\n", err)
		return
	}

	client := whatsmeow.NewClient(deviceStore, waLog.Noop)
	p.mu.Lock()
	p.waClient = client
	p.mu.Unlock()

	client.AddEventHandler(func(rawEvt interface{}) {
		switch rawEvt.(type) {
		case *events.Connected:
			fmt.Fprintln(os.Stderr, "   📲 [WHATSAPP BPR] ✅ Conectado a la red de WhatsApp Web.")
		case *events.LoggedOut:
			fmt.Fprintln(os.Stderr, "   📲 [WHATSAPP BPR] ⚠️ Sesión de WhatsApp cerrada en el teléfono.")
		}
	})

	if client.Store.ID == nil {
		// No session active: QR or pairing
		qrChan, _ := client.GetQRChannel(context.Background())
		err = client.Connect()
		if err != nil {
			fmt.Fprintf(os.Stderr, "[WHATSAPP BPR] Error conectando a WhatsApp: %v\n", err)
			return
		}

		go func() {
			firstQR := true
			for evt := range qrChan {
				if evt.Event == "code" {
					if qrCode, err := qr.Encode(evt.Code, qr.L); err == nil {
						_ = os.WriteFile("/home/dtp/solutions/RFID-mas-solution/whatsapp_qr.png", qrCode.PNG(), 0644)
					}

					fmt.Fprintln(os.Stderr, "\n=========================================================================================")
					fmt.Fprintln(os.Stderr, "   📲 [WHATSAPP BPR] ESCANEA ESTE CÓDIGO QR CON TU TELÉFONO (+34652948185):")
					fmt.Fprintln(os.Stderr, "   (Abre WhatsApp -> Menú ⋮ / Ajustes -> Dispositivos vinculados -> Vincular un dispositivo)")
					fmt.Fprintln(os.Stderr, "=========================================================================================")
					qrterminal.GenerateHalfBlock(evt.Code, qrterminal.L, os.Stderr)
					fmt.Fprintln(os.Stderr, "=========================================================================================")
					fmt.Fprintln(os.Stderr, "   🖼️ Imagen QR guardada en: /home/dtp/solutions/RFID-mas-solution/whatsapp_qr.png")

					if firstQR {
						firstQR = false
						time.Sleep(1 * time.Second)
						code, err := client.PairPhone(context.Background(), "34652948185", true, whatsmeow.PairClientChrome, "Chrome (Linux)")
						if err == nil && code != "" {
							fmt.Fprintln(os.Stderr, "-----------------------------------------------------------------------------------------")
							fmt.Fprintf(os.Stderr, "   🔑 CÓDIGO DE VINCULACIÓN NUMÉRICO (Alternativa al QR):  👉  %s  👈\n", code)
							fmt.Fprintln(os.Stderr, "   (En WhatsApp: Dispositivos vinculados -> Vincular con el número de teléfono -> Introduce este código)")
							fmt.Fprintln(os.Stderr, "-----------------------------------------------------------------------------------------\n")
						}
					}
				} else {
					fmt.Fprintf(os.Stderr, "   📲 [WHATSAPP BPR] Evento de vinculación: %s\n", evt.Event)
				}
			}
		}()
	} else {
		err = client.Connect()
		if err != nil {
			fmt.Fprintf(os.Stderr, "[WHATSAPP BPR] Error reconectando sesión previa: %v\n", err)
		} else {
			fmt.Fprintln(os.Stderr, "   📲 [WHATSAPP BPR] ✅ Sesión previa de WhatsApp Web restaurada y activa.")
		}
	}
}

func (p *Plugin) Handshake() sdk.HandshakeInfo {
	return sdk.HandshakeInfo{
		Inputs:          []string{"SendWhatsAppRequest"},
		Outputs:         []string{"SendWhatsAppResult"},
		Capabilities:    []string{},
		Language:        "go",
		SDKVersion:      "",
		ContractVersion: "bpr/v1",
	}
}

func (p *Plugin) OnTrigger(ctx *sdk.Context, trigger sdk.Trigger) ([]sdk.Message, error) {
	return nil, nil
}

func (p *Plugin) OnEvent(ctx *sdk.Context, event sdk.Event) ([]sdk.Message, error) {
	if event.MessageType != "SendWhatsAppRequest" {
		return nil, nil
	}

	payload := event.Payload
	phoneRaw, _ := payload["phone_number"].(string)
	customText, _ := payload["message_text"].(string)
	epcHex, _ := payload["epc_hex"].(string)
	gtin14, _ := payload["gtin14"].(string)
	serial, _ := payload["serial_number"].(string)
	readerMac, _ := payload["reader_mac"].(string)
	rssiVal := 0.0
	if r, ok := payload["rssi"].(float64); ok {
		rssiVal = r
	}
	distVal := 0.01
	if d, ok := payload["distance_m"].(float64); ok {
		distVal = d
	}

	// Hot-reload contacts from address book
	allContacts := p.loadContacts()

	// Determine targets
	var targetContacts []Contact
	phoneTrim := strings.TrimSpace(phoneRaw)
	if phoneTrim == "" || phoneTrim == "all" || phoneTrim == "*" || phoneTrim == "broadcast" {
		targetContacts = allContacts
	} else {
		// Specific numbers (comma separated or single)
		parts := strings.Split(phoneTrim, ",")
		for _, part := range parts {
			pt := strings.TrimSpace(part)
			if pt == "" {
				continue
			}
			found := false
			for _, c := range allContacts {
				if c.Phone == pt {
					targetContacts = append(targetContacts, c)
					found = true
					break
				}
			}
			if !found {
				targetContacts = append(targetContacts, Contact{
					Name:  "Usuario",
					Phone: pt,
					Lang:  detectLanguage(pt, ""),
				})
			}
		}
	}

	if len(targetContacts) == 0 {
		log.Printf("[WHATSAPP] ⚠️ No hay destinatarios configurados para enviar.")
		return nil, nil
	}

	nowIso := time.Now().UTC().Format(time.RFC3339)
	targetURL := p.webhookURL
	if cfgURL, ok := ctx.Config()["webhook_url"].(string); ok && cfgURL != "" {
		targetURL = cfgURL
	}

	var results []sdk.Message

	for _, contact := range targetContacts {
		cooldownKey := contact.Phone + ":" + epcHex

		p.mu.Lock()
		lastSent, exists := p.lastSentPerTag[cooldownKey]
		if epcHex != "" && exists && time.Since(lastSent) < p.cooldown {
			p.mu.Unlock()
			remaining := (p.cooldown - time.Since(lastSent)).Round(time.Second)
			log.Printf("[WHATSAPP ANTI-SPAM] 🛡️ Cooldown activo para %s (%s) con tag %s (restan %v). Silenciando spam.",
				contact.Name, contact.Phone, epcHex, remaining)
			continue
		}
		p.lastSentPerTag[cooldownKey] = time.Now()
		p.mu.Unlock()

		// Message content comes directly from AISemanticReactorSet (or fallback alert)
		var msgText string
		if customText != "" {
			msgText = customText
		} else {
			msgText = fmt.Sprintf("🎯 ¡Tag posado en antena Keonn Advan-CP1!\n• GTIN-14: %s\n• Serial: %s\n• EPC: %s\n• RSSI: %.1f dBm\n• Distancia: %.2f m\n• Lector MAC: %s",
				gtin14, serial, epcHex, rssiVal, distVal, readerMac)
		}

		msgID := fmt.Sprintf("wa-%d-%s", time.Now().UnixNano(), strings.TrimPrefix(contact.Phone, "+"))

		fmt.Fprintln(os.Stderr, "=========================================================================================")
		fmt.Fprintf(os.Stderr, "   📲 [WHATSAPP BPR PLUGIN] ¡DISPARO DE ALERTA TAG POSADO EN RADOMO!\n")
		fmt.Fprintf(os.Stderr, "   👤 Destinatario: %s (%s) | Idioma: %s\n", contact.Name, contact.Phone, contact.Lang)
		fmt.Fprintf(os.Stderr, "   🏷️ EPC Hex: %s\n", epcHex)
		fmt.Fprintf(os.Stderr, "   📦 GTIN-14: %s | Serial: %s\n", gtin14, serial)
		fmt.Fprintf(os.Stderr, "   📡 Lector MAC: %s | RSSI: %.1f dBm\n", readerMac, rssiVal)
		fmt.Fprintln(os.Stderr, "   📝 Contenido del mensaje WhatsApp traducido:")
		fmt.Fprintf(os.Stderr, "   %s\n", msgText)
		fmt.Fprintln(os.Stderr, "=========================================================================================")

		status := "SENT"
		var errMsg string

		p.mu.Lock()
		client := p.waClient
		p.mu.Unlock()

		if client != nil && client.IsConnected() && client.IsLoggedIn() {
			digits := ""
			for _, c := range contact.Phone {
				if c >= '0' && c <= '9' {
					digits += string(c)
				}
			}
			jid := types.NewJID(digits, types.DefaultUserServer)
			_, err := client.SendMessage(context.Background(), jid, &waE2E.Message{
				Conversation: proto.String(msgText),
			})
			if err != nil {
				status = "FAILED"
				errMsg = fmt.Sprintf("Error enviando por WhatsApp Web a %s: %v", contact.Phone, err)
				log.Printf("[WHATSAPP] ⚠️ %s", errMsg)
			} else {
				log.Printf("[WHATSAPP] ✅ Mensaje entregado a WhatsApp Web para %s (%s, Lang: %s)", contact.Name, contact.Phone, contact.Lang)
			}
			// Small pacing delay to avoid burst limits
			time.Sleep(300 * time.Millisecond)
		} else if targetURL != "" {
			bodyMap := map[string]any{
				"name":      contact.Name,
				"phone":     contact.Phone,
				"lang":      contact.Lang,
				"message":   msgText,
				"epc":       epcHex,
				"gtin14":    gtin14,
				"serial":    serial,
				"timestamp": nowIso,
			}
			jsonBody, _ := json.Marshal(bodyMap)
			httpClient := &http.Client{Timeout: 5 * time.Second}
			resp, err := httpClient.Post(targetURL, "application/json", bytes.NewReader(jsonBody))
			if err != nil {
				status = "FAILED"
				errMsg = fmt.Sprintf("Error enviando webhook WhatsApp a %s: %v", contact.Phone, err)
				log.Printf("[WHATSAPP] ⚠️ %s", errMsg)
			} else {
				defer resp.Body.Close()
				if resp.StatusCode >= 400 {
					status = "FAILED"
					errMsg = fmt.Sprintf("Webhook WhatsApp respondió HTTP %d para %s", resp.StatusCode, contact.Phone)
					log.Printf("[WHATSAPP] ⚠️ %s", errMsg)
				} else {
					log.Printf("[WHATSAPP] ✅ Mensaje entregado exitosamente a %s vía webhook %s", contact.Phone, targetURL)
				}
			}
		} else {
			log.Printf("[WHATSAPP] ℹ️ Plugin en modo auditoría local para %s (%s).", contact.Name, contact.Phone)
		}

		results = append(results, sdk.Message{
			Type: "SendWhatsAppResult",
			Payload: map[string]any{
				"status":        status,
				"recipient":     contact.Name,
				"phone_number":  contact.Phone,
				"language":      contact.Lang,
				"message_id":    msgID,
				"epc_hex":       epcHex,
				"error_message": errMsg,
				"timestamp":     nowIso,
			},
		})
	}

	return results, nil
}

func main() {
	plugin, err := NewPlugin()
	if err != nil {
		log.Fatalf("plugin init failed: %v", err)
	}
	if err := sdk.Run(plugin); err != nil {
		log.Fatalf("plugin runtime failed: %v", err)
	}
}
