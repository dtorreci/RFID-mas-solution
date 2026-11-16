# 📲 Bindu WhatsApp Plugin (BPR)

Plugin BPR (Bindu Plugin Runtime) para despacho y orquestación de alertas y notificaciones vía **WhatsApp Web** con persistencia SQLite y recarga en caliente de contactos.

---

## 🚀 Características Principales

1. **Transporte WhatsApp Web (whatsmeow)**:
   - Conexión cliente WebSocket directa con los servidores de WhatsApp mediante el protocolo Noise / Signal E2EE.
   - Almacenamiento seguro de claves de sesión en `/home/dtp/.bindu/root/runtime/data/whatsapp_web_session.db` (SQLite).
   - Reconexión y reanudación automática de sesión sin necesidad de reescanear el código QR al reiniciar el proceso.

2. **FileWatcher Reactivo con `sync.RWMutex` (`recipients.txt`)**:
   - Goroutine dedicada que monitorea cada 1 segundo el archivo de contactos ([recipients.txt](file:///home/dtp/.bindu/root/runtime/data/recipients.txt)).
   - Recarga en memoria atómica y thread-safe con `sync.RWMutex`:
     - Escritura en memoria protegida por `contactsMu.Lock()` (con debounce de 100 ms).
     - Lectura concurrente en despacho de eventos con `contactsMu.RLock()`.
   - Modificar, comentar o añadir números toma efecto en ~1 segundo sin reiniciar el plugin ni desconectar la sesión WhatsApp.

3. **Control Anti-Spam y Cooldown**:
   - Matriz en memoria con clave compuesta `phone + ":" + epc_hex`.
   - Cooldown configurable mediante variable de entorno `WHATSAPP_COOLDOWN_SEC` (por defecto 30s en pruebas, 10 min en producción).

4. **Contratos BPR**:
   - **Consume**: `SendWhatsAppRequest` (campos: `phone_number`, `message_text`, `epc_hex`, `gtin14`, `serial_number`, `rssi`, `reader_mac`, `timestamp`).
   - **Produce**: `SendWhatsAppResult` (estado de despacho, ID del mensaje y timestamp).

---

## 🛠️ Comandos de Certificación y Despliegue

```bash
# Certificar el plugin
bindu-studio plugin certify --source . --out ./bindu-whatsapp-plugin.tar.gz

# Desplegar al Gateway Root
bindu-studio plugin apply --bundle ./bindu-whatsapp-plugin.tar.gz --target root --auth <user>:<pass>
```
