import urllib.request
import os

url = 'http://192.168.1.86/cgi-bin/mqttsettings.cgi'
reader_user = os.getenv('READER_USER', '<reader_user>')
reader_pass = os.getenv('READER_PASS', '<reader_password>')
pwd_mgr = urllib.request.HTTPPasswordMgrWithDefaultRealm()
pwd_mgr.add_password('Sensormatic', url, reader_user, reader_pass)
handler = urllib.request.HTTPDigestAuthHandler(pwd_mgr)
opener = urllib.request.build_opener(handler)

certs_dir = '/home/dtp/solutions/RFID-mas-solution/reader_certs'
with open(os.path.join(certs_dir, 'ca.crt'), 'rb') as f:
    ca_bytes = f.read()
with open(os.path.join(certs_dir, 'idx4000.crt'), 'rb') as f:
    cert_bytes = f.read()
with open(os.path.join(certs_dir, 'idx4000.key'), 'rb') as f:
    key_bytes = f.read()

boundary = '----WebKitFormBoundaryBinduIDX4000Setup'

fields = [
    ('TopicVersion', ''),
    ('MQTT_EnabledState', '1'),
    ('MQTTEnable', 'on'),
    ('MQTT_Hostname', '192.168.1.75'),
    ('MQTT_Port', '1883'),
    ('MQTT_Topic_Version', '0'),
    ('MQTT_BT_ClientID', '0000idx'),
    ('MQTT_BT_Region', 'eu'),
    ('MQTT_BT_StoreID', 'bindu1'),
    ('MQTT_BT_ProdCat', '0000rfid'),
    ('MQTT_BT_GroupID', 'group1'),
    ('MQTT_BT_ReaderID', 'reader1'),
    ('MQTT_BaseTopic', 'bindu/in/raw/rfid/idx4000'),
    ('MQTT_POS_BaseTopic', '{null}'),
    ('MQTT_NeedAuth', '1'),
    ('MqttNeedAuth', 'on'),
    ('MQTT_Username', os.getenv('MQTT_USER', '<user>')),
    ('MQTT_Passwd', os.getenv('MQTT_PASS', '<password>')),
    ('MQTT_TLSMode', '2'),
    ('MQTT_ClientKeyPassword', ''),
    ('ApplyBtn', 'Apply'),
]

files = [
    ('CACertFile', 'ca.crt', ca_bytes),
    ('ClientCertFile', 'idx4000.crt', cert_bytes),
    ('ClientKeyFile', 'idx4000.key', key_bytes),
]

body = bytearray()
for name, val in fields:
    body.extend(f'--{boundary}\r\n'.encode('utf-8'))
    body.extend(f'Content-Disposition: form-data; name="{name}"\r\n\r\n'.encode('utf-8'))
    body.extend(f'{val}\r\n'.encode('utf-8'))

for name, filename, content in files:
    body.extend(f'--{boundary}\r\n'.encode('utf-8'))
    body.extend(f'Content-Disposition: form-data; name="{name}"; filename="{filename}"\r\n'.encode('utf-8'))
    body.extend(b'Content-Type: application/x-x509-ca-cert\r\n\r\n')
    body.extend(content)
    body.extend(b'\r\n')

body.extend(f'--{boundary}--\r\n'.encode('utf-8'))

req = urllib.request.Request(url, data=bytes(body), method='POST')
req.add_header('Content-Type', f'multipart/form-data; boundary={boundary}')

try:
    print("Enviando configuración MQTT y certificados al lector IDX-4000...")
    res = opener.open(req, timeout=10)
    print("✔ Respuesta HTTP del lector:", res.status)
    resp_text = res.read().decode('latin1', errors='ignore')
    if "reboot" in resp_text.lower() or "done" in resp_text.lower() or res.status == 200:
        print("✔ Configuración MQTT aplicada con éxito en el lector.")
    else:
        print("Respuesta:", resp_text[:500])
except Exception as e:
    print("❌ Error configurando lector:", e)
