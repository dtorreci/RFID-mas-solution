# 📦 Paquete de Instalación: Bindu Customer App para Sensormatic IDX-4000

Este paquete contiene todo lo necesario para incorporar nuevos lectores RFID **Sensormatic IDX-4000** a la malla Multi-Agente de **Bindu (v0.27.520)** con **Zero-Touch Provisioning**.

---

## 📂 Contenido del Paquete

| Archivo | Descripción |
| :--- | :--- |
| `idx4000_edge_agent` | Binario nativo compilado en Linux ARMv7 (LLRP + mTLS + Auto-Enrollment PKCS#10). |
| `customerApp` | Script supervisor que arranca automáticamente al iniciar el lector. |
| `deploy.py` | Script de despliegue automático multiplataforma (Windows / Linux / macOS). |
| `deploy.sh` | Wrapper en Bash para entornos Linux/macOS. |

---

## 🚀 Guía de Instalación en 2 Pasos

### PASO 1: Registrar el Lector en Bindu Root (1 minuto)

Antes de conectar el lector por primera vez, el administrador de Bindu debe autorizar su identificador en la topología de la red:

1. Obtener la dirección MAC del nuevo lector (impresa en la etiqueta de código de barras del dispositivo o visible en el router DHCP, ej: `00:50:F9:88:99:AA`).
2. En el servidor central Bindu Root (`bindu.decn.casa`), añadir el nodo al archivo `/home/dtp/.bindu/root/runtime/config/mesh.json` dentro de `root.children`:

```json
{
  "id": "idx4000-00-50-f9-88-99-aa",
  "name": "IDX4000-0050F98899AA",
  "slug": "idx4000-00-50-f9-88-99-aa",
  "connectivity_kind": "gateway",
  "platform": {
    "os": "linux",
    "arch": "armv7"
  },
  "plugins": [
    "bindu-idx4000-plugin"
  ],
  "bindu": {
    "node_id": "idx4000-00-50-f9-88-99-aa",
    "channels": {
      "mesh_mqtt": { "port": 1883 },
      "mqtt_integration": { "port": 1885 }
    }
  }
}
```

---

### PASO 2: Instalar la Customer App en el Lector (2 minutos)

El instalador en campo sólo necesita estar en la misma red local que el lector nuevo.

#### Opción A: Despliegue Automático (Recomendado)
Ejecutar desde cualquier ordenador (requiere Python 3):
```bash
python3 deploy.py 192.168.1.150
```
*(Sustituir `192.168.1.150` por la IP asignada al nuevo lector).*

#### Opción B: Instalación Manual vía FTP (Sin scripts)
Si el técnico prefiere usar un cliente gráfico como **FileZilla** o **WinSCP**:
1. Conectar por FTP a la IP del lector (Puerto `21`):
   - **Usuario**: `admin`
   - **Contraseña**: `USBB24`
2. Copiar los archivos `customerApp` e `idx4000_edge_agent` a la raíz del FTP (que corresponde a `/customer_apps/`).
3. Reiniciar el lector (apagando y encendiendo el interruptor físico).

---

## ⚡ ¿Qué sucede al arrancar el lector?

1. **Detección Automática**: El firmware del lector detecta `/customer_apps/customerApp` y lo lanza como servicio supervisor.
2. **Identidad Inmutable**: El agente lee su dirección MAC física (`eth0`) y genera su NodeID (`idx4000-MAC`).
3. **Preflight & Auto-Enrollment (Bindu 0.27.520)**:
   - Contacta con `POST https://bindu.decn.casa:8444/api/v1/tls/preflight-node`.
   - Genera localmente su clave RSA-2048 y emite una solicitud CSR PKCS#10.
   - Recibe y almacena su certificado mTLS en `/customer_apps/tls/server.crt`.
4. **Operación en Vivo**:
   - Conecta a LLRP local (`127.0.0.1:5084`), fija la antena 1 a 18 dBm.
   - Establece el enlace cifrado mTLS con Bindu Root (`bindu.decn.casa:1883`).
   - Comienza a emitir `RawTagRead`, `NodePresence`, `MessageState` y `MessagePing`.

---

## 🔍 Verificación y Logs

Para verificar la actividad del lector, se puede consultar el log en tiempo real por FTP descargando `/customer_apps/edge_agent.log` o por Telnet:

```bash
# Ver últimas líneas de ejecución en el lector
tail -f /customer_apps/edge_agent.log
```
