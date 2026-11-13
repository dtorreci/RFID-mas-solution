package main

import (
	"crypto/tls"
	"encoding/binary"
	"fmt"
	"os"
	"strconv"
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

	// 1. MQTT CONNECT packet (v3.1.1)
	user := os.Getenv("MQTT_USER")
	pass := os.Getenv("MQTT_PASS")
	clientID := "test-agent-probe"

	var varHdr []byte
	varHdr = append(varHdr, 0x00, 0x04, 'M', 'Q', 'T', 'T', 0x04) // Protocol Name & Level 4
	connectFlags := byte(0x02)                                     // Clean Session
	if user != "" {
		connectFlags |= 0x80
	}
	if pass != "" {
		connectFlags |= 0x40
	}
	varHdr = append(varHdr, connectFlags, 0x00, 0x3C) // Keepalive 60s

	var payload []byte
	payload = append(payload, encodeString(clientID)...)
	if user != "" {
		payload = append(payload, encodeString(user)...)
	}
	if pass != "" {
		payload = append(payload, encodeString(pass)...)
	}

	connectPkt := buildMQTTPacket(0x10, append(varHdr, payload...))
	conn.Write(connectPkt)

	// Read CONNACK
	ack := make([]byte, 4)
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := conn.Read(ack); err != nil {
		fmt.Printf("CONNACK read error: %v\n", err)
		return
	}
	if ack[0] != 0x20 || ack[3] != 0x00 {
		fmt.Printf("CONNACK failed: header=0x%x return_code=%d\n", ack[0], ack[3])
		return
	}
	fmt.Println("✔ MQTT Connected successfully as dtorreci to localhost:1883!")

	rssiVal := 60.0
	if len(os.Args) > 1 {
		if v, err := strconv.ParseFloat(os.Args[1], 64); err == nil {
			rssiVal = v
		}
	}

	// 2. Publish EpcSgtinRead test event to bindu/in
	topic := "bindu/in"
	nowStr := time.Now().UTC().Format(time.RFC3339)
	eventJSON := fmt.Sprintf(`{
		"$type": "EpcSgtinRead",
		"$group": "mesh",
		"$node-dest": "root",
		"$producer": "rfid-bridge",
		"$user": "dtorreci",
		"$when": "%s",
		"fields": {
			"epc_hex": "30b5d132b8112b42f9b59b2a",
			"standard": "SGTIN-96",
			"company_prefix": "7621806",
			"item_reference": "017581",
			"antenna_id": 1,
			"channel_index": 1,
			"peak_rssi_dbm": %.1f,
			"gtin14": "07621806175810",
			"serial_number": "12779363114",
			"reader_id": "portal-reader-01",
			"canonical_urn": "urn:epc:tag:sgtin-96:5.7621806.017581.12779363114",
			"first_seen_utc": "%s",
			"last_seen_utc": "%s"
		}
	}`, nowStr, rssiVal, nowStr, nowStr)

	var pubVarHdr []byte
	pubVarHdr = append(pubVarHdr, encodeString(topic)...)
	pubPkt := buildMQTTPacket(0x30, append(pubVarHdr, []byte(eventJSON)...)) // QoS 0
	conn.Write(pubPkt)

	fmt.Println("✔ Published EpcSgtinRead event to topic 'bindu/in'.")
	time.Sleep(300 * time.Millisecond)
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
