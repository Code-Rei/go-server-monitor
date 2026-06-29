<div align="center">
  <h1>🖥️ Server Monitor</h1>
  <p>Lightweight, real-time server monitoring written in Go.</p>
  
  ![Go](https://img.shields.io/badge/Go-1.26+-00ADD8?style=for-the-badge&logo=go)
  ![Docker](https://img.shields.io/badge/Docker-Ready-2496ED?style=for-the-badge&logo=docker)
  ![Size](https://img.shields.io/badge/Size-~6MB-success?style=for-the-badge)
  ![RAM](https://img.shields.io/badge/RAM-~10MB-blueviolet?style=for-the-badge)
</div>

<br/>

## ✨ Features

- **Blazing Fast**: Native Go performance with concurrent metric collection.
- **Ultra Lightweight**: Uses just ~10 MB of RAM compared to Node.js (~150 MB).
- **Single Binary**: No dependencies. The entire dashboard UI is embedded!
- **Cross-Platform**: Runs on Windows (native `.exe`) and Linux (via Docker).
- **Hardware Temperatures**: Supports Windows WMI and Linux `sysfs` thermal sensors.

---

## 📡 Endpoints

| Endpoint | Description |
|----------|-------------|
| `GET /` | Live dashboard UI (auto-refreshes every 5s) |
| `GET /metrics` | Compact JSON response (original format) |
| `GET /metrics/full` | Extended JSON with detailed hardware info |

<details>
<summary><b>Click to view <code>/metrics</code> JSON example</b></summary>

```json
{
  "cpu": 2.1,
  "ram": 54.8,
  "load": 0.25,
  "temp": 52,
  "rx": 7320,
  "tx": 979,
  "uptime": 851047
}
```

</details>

---

## 🪟 Windows Setup (No Docker Required)

You don't need Docker or Go to run this on Windows. It works out of the box.

1. **Build the `.exe`** (only needed once):
   ```powershell
   .\build.ps1
   ```
2. **Run it**:
   ```powershell
   .\server-monitor.exe
   ```
3. **View Dashboard**: Open [http://localhost:8266](http://localhost:8266)

> 💡 **Temperature Tip:** To view CPU temperatures on Windows, you must right-click `server-monitor.exe` and select **"Run as administrator"**. This grants access to the WMI hardware sensors.

---

## 🐳 Docker Setup (Linux / Ubuntu)

### 1. Load the Image
If you were provided a `.tar` file:
```bash
docker load -i server-monitor.tar
```

### 2. Run the Container
Choose the command that best fits your environment:

**A. Basic Run** (Temp will be `null` on VPS/VMs)
```bash
docker run -d \
  --name server-monitor \
  --restart unless-stopped \
  -p 8266:8266 \
  server-monitor
```

**B. Hardware Sensors (Bare-Metal Linux)**
```bash
# Recommended for full thermal sensor access
docker run -d \
  --name server-monitor \
  --restart unless-stopped \
  --privileged \
  -p 8266:8266 \
  server-monitor
```

**C. Sensor Mounts Only** (Safer alternative to `--privileged`)
```bash
docker run -d \
  --name server-monitor \
  --restart unless-stopped \
  -v /sys/class/thermal:/sys/class/thermal:ro \
  -v /sys/class/hwmon:/sys/class/hwmon:ro \
  -p 8266:8266 \
  server-monitor
```

### 3. Useful Docker Commands
```bash
docker logs -f server-monitor    # View live logs
docker restart server-monitor    # Restart the service
docker rm -f server-monitor      # Remove the container entirely
```

---

## 📊 Available Metrics

| Metric | Description |
|--------|-------------|
| `cpu` | Total CPU usage % |
| `ram` | RAM usage % |
| `load` | 1-min load average |
| `temp` | CPU temp °C (returns `null` if no physical sensor is available) |
| `rx` / `tx` | Network traffic (bytes/sec) |
| `uptime` | System uptime in seconds |
| `memory` | Used / Free / Total in GB |
| `swap` | Page file / swap usage |
| `disk` | Primary disk usage |
| `network` | Per-interface bandwidth statistics |
| `cpuInfo` | Model name, cores, threads, and per-core % |
| `system` | Hostname, OS, and Architecture |
| `processes` | Total running process count |

---

## ⚡ Resource Comparison (Legacy Node.js vs New Go)

| Runtime | RAM usage | Image size | Executable | Temp on Windows |
|---------|-----------|------------|------------|-----------------|
| 🔴 **Old (Node.js)** | ~100-150 MB | ~200 MB | Requires Node | ❌ Complicated |
| 🟢 **New (Go)** | **~10 MB** | **~15 MB** | **Standalone `.exe`** | ✅ Built-in (via WMI) |
