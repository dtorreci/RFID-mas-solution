package main

import (
	"crypto/tls"
	"encoding/binary"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	cert, err := tls.LoadX509KeyPair("/home/dtp/.bindu/root/runtime/tls/root.crt", "/home/dtp/.bindu/root/runtime/tls/root.key")
	if err != nil {
		fmt.Printf("Cert load error: %v\n", err)
		return
	}
	tlsConf := &tls.Config{
		Certificates:       []tls.Certificate{cert},
		InsecureSkipVerify: true,
	}
	conn, err := tls.Dial("tcp", "127.0.0.1:1883", tlsConf)
	if err != nil {
		fmt.Printf("Dial error: %v\n", err)
		return
	}
	defer conn.Close()

	user := "dtorreci"
	pass := "BinduMaster2026!"
	clientID := "whatsapp-live-tester"

	var varHdr []byte
	varHdr = append(varHdr, 0x00, 0x04, 'M', 'Q', 'T', 'T', 0x04)
	connectFlags := byte(0x02 | 0x80 | 0x40) // Clean session + user + pass
	varHdr = append(varHdr, connectFlags, 0x00, 0x3C)

	var payload []byte
	payload = append(payload, encodeString(clientID)...)
	payload = append(payload, encodeString(user)...)
	payload = append(payload, encodeString(pass)...)

	connectPkt := buildMQTTPacket(0x10, append(varHdr, payload...))
	conn.Write(connectPkt)

	ack := make([]byte, 4)
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := conn.Read(ack); err != nil {
		fmt.Printf("CONNACK read error: %v\n", err)
		return
	}
	conn.SetReadDeadline(time.Time{})
	if ack[0] != 0x20 || ack[3] != 0x00 {
		fmt.Printf("CONNACK failed: 0x%x return_code=%d\n", ack[0], ack[3])
		return
	}
	fmt.Println("🚀 [TEST LOOP] Conectado exitosamente al broker Bindu (127.0.0.1:1883). Iniciando ráfaga de 1 msg/seg...")

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	count := 0
	topic := "bindu/in"

	for {
		select {
		case <-sigChan:
			fmt.Println("\n⏹️ [TEST LOOP] Detenido por señal de usuario.")
			return
		default:
		}

		count++
		now := time.Now()
		tsHuman := now.Format("15:04:05")
		tsISO := now.UTC().Format(time.RFC3339)
		msgText := fmt.Sprintf("Prueba %s (#%d)", tsHuman, count)

		eventJSON := fmt.Sprintf(`{
			"$type": "SendWhatsAppRequest",
			"$group": "mesh",
			"$node-dest": "root",
			"$producer": "test-loop",
			"$user": "dtorreci",
			"$when": "%s",
			"fields": {
				"phone_number": "all",
				"message_text": "%s",
				"epc_hex": "",
				"gtin14": "TEST-LOOP",
				"serial_number": "%d",
				"rssi": 60.0,
				"reader_mac": "00:50:F9:D1:0A:2B",
				"timestamp": "%s"
			}
		}`, tsISO, msgText, count, tsISO)

		var pubVarHdr []byte
		pubVarHdr = append(pubVarHdr, encodeString(topic)...)
		pubPkt := buildMQTTPacket(0x30, append(pubVarHdr, []byte(eventJSON)...))
		if _, err := conn.Write(pubPkt); err != nil {
			fmt.Printf("❌ Error publicando mensaje #%d: %v\n", count, err)
			return
		}

		fmt.Printf("[LOOP #%d %s] 📤 Publicado a Bindu: '%s'\n", count, tsHuman, msgText)
		time.Sleep(1 * time.Second)
	}
}

func encodeString(s string) []byte {
	buf := make([]byte, 2+len(s))
	binary.BigEndian.PutUint16(buf[0:2], uint16(len(s)))
	copy(buf[2:], []byte(s))
	return buf
}

func buildMQTTPacket(pktType byte, data []byte) []byte {
	var remLen []byte
	x := len(data)
	for {
		encodedByte := byte(x % 128)
		x = x / 128
		if x > 0 {
			encodedByte |= 128
		}
		remLen = append(remLen, encodedByte)
		if x == 0 {
			break
		}
	}
	var pkt []byte
	pkt = append(pkt, pktType)
	pkt = append(pkt, remLen...)
	pkt = append(pkt, data...)
	return pkt
}
