# Sensormatic IDX-4000 Native RFID Edge Controller (BPR Plugin)

Plugin BPR nativo desarrollado para ejecutarse directamente en la CPU Freescale i.MX6 (ARMv7 Cortex-A7) del lector RFID **Sensormatic IDX-4000**, gobernado por un **Bindu Node** conectado en topología Mesh mTLS con el **Bindu Root**.

---

## 🎯 Capacidades y Requisitos Cumplidos

1. **Strong Identity en el Mesh**:
   - Identidad canónica asignada estrictamente a partir de la dirección MAC física del dispositivo: `idx4000-00-50-f9-d1-0a-2b` (`00:50:f9:d1:0a:2b`).
2. **Cero Conexiones Directas a MQTT**:
   - Toda la telemetría, directivas de control y orquestación fluyen a través de la infraestructura nativa de Bindu (Mesh mTLS, Bindu Studio, plugins BPR y reactores CEL declarativos).
3. **Control Bidireccional Dinámico (`RfidReaderConfig`)**:
   - **Ajuste de Potencia**: `tx_power_cdbm` / `tx_power_dbm` (e.g., 18 dBm, 24 dBm, 30 dBm).
   - **Umbral de Sensibilidad / RSSI Cutoff**: `rssi_threshold_dbm` (filtrado en origen de lecturas de baja potencia).
   - **Conmutación de Filtro de Tags**: `filter_tags` (`false` para lectura total de tags del entorno, `true` para aislar tags objetivo específicos).
   - **Modos de Ejecución de la ROSpec**:
     - `INFINITE` / `CONTINUOUS`: La ROSpec se ejecuta ininterrumpidamente radiando RF.
     - `PERIODIC` / `CYCLE`: Ejecuta lectura activa durante una ventana de $X$ segundos (`window_duration_sec`), duerme durante $Y$ segundos (`cycle_interval_sec`) y repite indefinidamente.
     - `ADHOC` / `ON_DEMAND`: El lector permanece en reposo (standby con radio apagada). Al recibir una orden desde Bindu (`command: "TRIGGER_ADHOC"`), activa la lectura durante la ventana especificada y se detiene automáticamente volviendo a standby.

---

## 📐 Mensajería Bindu

- **Entrada (Consumes)**: `RfidReaderConfig`
  - Routing: `["local", "child"]` (despachado desde Root hacia el nodo del lector en el mesh).
- **Salida (Produces)**: `RawTagRead`
  - Routing: `["local", "parent"]` (enrutado automáticamente hacia Root para su ingesta en el reactor de cinemática y proximidad).

---

## 🛠 Compilación y Certificación

```bash
# Certificación oficial y compilación cruzada multitarjet (ARMv7 / AMD64)
bindu-studio plugin certify --source ./bindu-idx4000-plugin --targets-from-mesh --out ./bindu-idx4000-plugin.tar.gz

# Despliegue hacia el lector
bindu-studio plugin apply --bundle ./bindu-idx4000-plugin.tar.gz --target node --node idx4000-00-50-f9-d1-0a-2b --auth <user>:<password>
```
