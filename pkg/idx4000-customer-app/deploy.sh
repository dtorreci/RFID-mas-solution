#!/usr/bin/env bash
# ==============================================================================
# SENSORMATIC IDX-4000 - ONE-CLICK DEPLOY SCRIPT (BASH)
# Uso: ./deploy.sh <IP_DEL_LECTOR>
# ==============================================================================

set -e

READER_IP="$1"
FTP_USER="${FTP_USER:-admin}"
FTP_PASS="${FTP_PASS:-USBB24}"

if [ -z "$READER_IP" ]; then
    echo "Uso: $0 <IP_DEL_LECTOR>"
    echo "Ejemplo: $0 192.168.1.150"
    exit 1
fi

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

echo "=== Subiendo Bindu Customer App a $READER_IP vía FTP ==="
curl -u "${FTP_USER}:${FTP_PASS}" -T "${SCRIPT_DIR}/customerApp" "ftp://${READER_IP}/customerApp.new"
curl -u "${FTP_USER}:${FTP_PASS}" -T "${SCRIPT_DIR}/idx4000_edge_agent" "ftp://${READER_IP}/idx4000_edge_agent.new"

echo "=== Renombrando archivos en el lector ==="
python3 -c "
import ftplib
ftp = ftplib.FTP('${READER_IP}')
ftp.login('${FTP_USER}', '${FTP_PASS}')
for f in ['customerApp', 'idx4000_edge_agent']:
    try: ftp.delete(f)
    except: pass
    ftp.rename(f + '.new', f)
ftp.quit()
"

echo "=== Asignando permisos y ejecutando ==="
python3 "${SCRIPT_DIR}/deploy.py" "${READER_IP}" --ftp-user "${FTP_USER}" --ftp-pass "${FTP_PASS}"
