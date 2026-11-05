# UET Cloud - Container Load Tester (`uet-cloud-test`)

Công cụ tạo tải CPU & RAM chuyên dụng cho container, tối ưu để kiểm thử nền tảng PaaS **uet-cloud** (Azure Container Apps - ACA).

---

## 🌟 Tính năng nổi bật

1. **Kiểm soát linh hoạt % CPU & RAM**:
   - Cho phép điều chỉnh mức tải chính xác từ **0% đến 100%** theo cgroup limit được cấp cho container.
   - Hỗ trợ nhập số **vCPU** hoặc dung lượng **MB RAM** tuyệt đối.
2. **Tự động nhận diện Container Cgroup (v1 & v2)**:
   - Tự động đọc CPU quota/period (`/sys/fs/cgroup/cpu.max` hoặc `cpu.cfs_quota_us`).
   - Tự động đọc Memory limit (`/sys/fs/cgroup/memory.max` hoặc `memory.limit_in_bytes`).
   - Nếu chạy ngoài container hoặc không giới hạn, ứng dụng tự động fallback theo tài nguyên Host.
3. **Cơ chế đo lường & sinh tải chuẩn xác**:
   - **CPU Stress**: Dùng thuật toán duty-cycle spinning kết hợp đa goroutine theo chu kỳ 100ms, giúp CPU tăng đều và ổn định mà không bị kernel throttle bất thường.
   - **RAM Stress**: Cấp phát memory chunk và ghi đè từng trang (4096 bytes) để buộc kernel Linux tính vào **RSS / WorkingSetBytes** (khớp với số liệu Azure Monitor `WorkingSetBytes`).
   - **Xả RAM tức thì**: Khi bấm Stop/Reset, bộ nhớ được hủy tham chiếu và kích hoạt `runtime.GC()` kèm `debug.FreeOSMemory()` để trả RAM ngay về cho OS/cgroup.
4. **WebUI tích hợp (Zero-dependency)**:
   - Giao diện Dark theme hiện đại, responsive, nhúng sẵn trong binary Go (không cần kết nối internet hay CDN ngoài).
   - Tự động polling và cập nhật chỉ số CPU%, RAM (MB) thực tế của container mỗi 1.5 giây.
   - Hỗ trợ **Auto-stop Timer** (30s, 1m, 3m, 5m) để phòng tránh OOM-kill hoặc quên tắt tải.
5. **Siêu nhẹ & Bảo mật**:
   - Docker image tối ưu chỉ **~15MB**.
   - Mức tiêu thụ nền (Idle): **~0% CPU**, **~5MB RAM**.
   - Chạy dưới user phi root (`appuser: 10001:10001`).

---

## 🚀 Hướng dẫn Chạy Thử Nghiệm

### Cách 1: Chạy trực tiếp bằng Go (Local)

```bash
cd tmp/uet-cloud-test
go run main.go
# Mở trình duyệt tại http://localhost:8080
```

### Cách 2: Chạy bằng Docker (Giả lập giới hạn tài nguyên như trên ACA)

```bash
# Build image
docker build -t uet-cloud-test:latest tmp/uet-cloud-test

# Chạy với giới hạn 1.0 vCPU và 512MB RAM
docker run -d --name uet-test -p 8080:8080 --cpus=1.0 --memory=512m uet-cloud-test:latest

# Kiểm tra stats thực tế từ Docker
docker stats uet-test
```

### Cách 3: Chạy bằng Docker Compose

```bash
cd tmp/uet-cloud-test
docker compose up -d
```

---

## 📡 REST API Tham chiếu

Bên cạnh WebUI, bạn có thể điều khiển tải bằng REST API (dùng curl hoặc script automation):

### 1. `GET /api/status`
Lấy thông tin cgroup, cấu hình tải mục tiêu và số liệu thời gian thực của container:
```bash
curl -s http://localhost:8080/api/status | jq
```
*Phản hồi mẫu:*
```json
{
  "limits": {
    "cgroup_version": "v2",
    "cpu_limit": 1,
    "memory_limit_bytes": 536870912,
    "host_cpus": 8,
    "host_memory_bytes": 16777216000
  },
  "metrics": {
    "cpu_usage_percent": 50.2,
    "memory_rss_bytes": 268435456,
    "memory_rss_mb": 256.0,
    "memory_usage_percent": 50.0
  },
  "is_active": true,
  "target_cpu_percent": 50,
  "target_memory_percent": 50,
  "target_memory_mb": 256,
  "duration_seconds": 60,
  "remaining_seconds": 45
}
```

### 2. `POST /api/load`
Kích hoạt hoặc cập nhật mức tải:
```bash
curl -X POST http://localhost:8080/api/load \
  -H "Content-Type: application/json" \
  -d '{
    "cpu_percent": 60,
    "memory_percent": 70,
    "duration_seconds": 120
  }'
```

### 3. `POST /api/stop`
Dừng toàn bộ worker CPU và giải phóng RAM:
```bash
curl -X POST http://localhost:8080/api/stop
```

### 4. `GET /healthz`
Health check endpoint cho Container Apps probe:
```bash
curl -s http://localhost:8080/healthz
```

---

## 🎯 Ứng dụng kiểm thử với uet-cloud (Kumo)

1. **Deploy lên Kumo**: Đẩy repository này lên GitHub cá nhân, sau đó kết nối và deploy qua Kumo PaaS với cấu hình CPU/RAM mong muốn (ví dụ: `0.5 vCPU`, `1 GiB RAM`).
2. **Kích hoạt tải**: Mở URL FQDN của ứng dụng, kéo thanh trượt CPU lên 70% và RAM lên 60%.
3. **Đối chiếu Monitoring API**: Gọi endpoint `GET /deployments/{id}/metrics?range=1h` trên backend Kumo để kiểm tra biểu đồ metrics `UsageNanoCores` và `WorkingSetBytes` khớp với mức tải đã điều khiển.
