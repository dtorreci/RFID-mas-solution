#!/usr/bin/env python3
"""
Sensormatic IDX-4000 - Bindu Customer App One-Click Deployment Tool
Multiplataforma: Windows, Linux, macOS (Sólo requiere Python 3.6+)
"""

import sys
import os
import time
import ftplib
import socket
import argparse

def log(msg, level="INFO"):
    colors = {
        "INFO": "\033[94m[INFO]\033[0m",
        "OK": "\033[92m[OK]\033[0m",
        "WARN": "\033[93m[WARN]\033[0m",
        "ERR": "\033[91m[ERROR]\033[0m"
    }
    prefix = colors.get(level, f"[{level}]")
    print(f"{prefix} {msg}")

def upload_via_ftp(reader_ip, ftp_user, ftp_pass, pkg_dir):
    log(f"Conectando a {reader_ip}:21 vía FTP...")
    ftp = ftplib.FTP(timeout=15)
    ftp.connect(reader_ip, 21)
    ftp.login(ftp_user, ftp_pass)
    log("Autenticación FTP correcta.", "OK")

    files_to_deploy = ["customerApp", "idx4000_edge_agent"]
    for fname in files_to_deploy:
        local_path = os.path.join(pkg_dir, fname)
        if not os.path.exists(local_path):
            log(f"Falta el archivo {fname} en {pkg_dir}", "ERR")
            sys.exit(1)

        size_mb = os.path.getsize(local_path) / (1024 * 1024)
        log(f"Subiendo {fname} ({size_mb:.2f} MB)...")
        tmp_name = f"{fname}.new"
        with open(local_path, "rb") as f:
            ftp.storbinary(f"STOR {tmp_name}", f)
        
        # Renombrado atómico POSIX para evitar ETXTBSY
        try:
            ftp.delete(fname)
        except Exception:
            pass
        ftp.rename(tmp_name, fname)
        log(f"Archivo {fname} subido y renombrado exitosamente.", "OK")

    ftp.quit()

def activate_via_telnet(reader_ip, reboot=False):
    log("Conectando vía Telnet (puerto 23) para activar permisos y servicio...")
    s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    s.settimeout(10)
    try:
        s.connect((reader_ip, 23))
        time.sleep(1)
        data = s.recv(1024).decode("latin1", "ignore")
        
        # Intento de login root o trivelp
        if "login" in data.lower():
            s.sendall(b"root\n")
            time.sleep(0.5)
            s.sendall(b"sensormatic\n")
            time.sleep(1)
        elif "trivelp" in data.lower():
            s.sendall(b"trivelp\r\n")
            time.sleep(0.5)
            s.sendall(b"USBB24\r\n")
            time.sleep(0.5)
            s.sendall(b"p\r\n")
            time.sleep(1)

        log("Asignando permisos ejecutables (chmod 777)...")
        s.sendall(b"chmod 777 /customer_apps/customerApp /customer_apps/idx4000_edge_agent\n")
        time.sleep(0.5)

        if reboot:
            log("Reiniciando el lector para arranque limpio del firmware...", "WARN")
            s.sendall(b"reboot\n")
            time.sleep(1)
        else:
            log("Deteniendo instancias previas y arrancando customerApp...")
            s.sendall(b"killall -q idx4000_edge_agent customerApp; sleep 1; /customer_apps/customerApp &\n")
            time.sleep(2)
        
        s.close()
        log("Servicio activado con éxito en el lector.", "OK")
    except Exception as e:
        log(f"No se pudo completar activación por Telnet ({e}).", "WARN")
        log("Nota: Si el lector se reinicia físicamente, el firmware iniciará customerApp automáticamente.", "INFO")

def main():
    parser = argparse.ArgumentParser(description="Instalador del paquete Bindu Customer App para Sensormatic IDX-4000")
    parser.add_argument("reader_ip", help="Dirección IP del nuevo lector IDX-4000 (ej: 192.168.1.150)")
    parser.add_argument("--ftp-user", default="admin", help="Usuario FTP (por defecto: admin)")
    parser.add_argument("--ftp-pass", default="USBB24", help="Contraseña FTP (por defecto: USBB24)")
    parser.add_argument("--reboot", action="store_true", help="Reiniciar el lector tras la instalación")
    args = parser.parse_args()

    pkg_dir = os.path.dirname(os.path.abspath(__file__))
    print("=====================================================================")
    print(" 📡 BINDU MAS - SENSORMATIC IDX-4000 CUSTOMER APP DEPLOYMENT")
    print(f" Destino: {args.reader_ip}")
    print("=====================================================================")

    try:
        upload_via_ftp(args.reader_ip, args.ftp_user, args.ftp_pass, pkg_dir)
        activate_via_telnet(args.reader_ip, reboot=args.reboot)
        print("=====================================================================")
        log("¡Despliegue completado!", "OK")
        log("El lector se auto-enrolará en Bindu Root en su primer arranque.", "INFO")
        log(f"Puedes consultar sus logs conectando por FTP a {args.reader_ip} y leyendo 'edge_agent.log'.", "INFO")
        print("=====================================================================")
    except Exception as err:
        log(f"Fallo durante el despliegue: {err}", "ERR")
        sys.exit(1)

if __name__ == "__main__":
    main()
