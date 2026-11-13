# UET Cloud - Container & Traffic Load Tester (`uet-cloud-test`)

Công cụ tạo tải CPU, RAM và **bắn tải HTTP Concurrency** chuyên dụng, tối ưu hóa để kiểm thử cơ chế tự động mở rộng (Autoscaling) và giới hạn tài nguyên của **Azure Container Apps (ACA)**.

---

## 🌟 Tính năng nổi bật

### 1. Tự động nhận diện vCPU động (Dynamic Cgroup Resolution)
- **Khắc phục nhận sai vCPU trên Azure**: Azure Container Apps chạy trên các node VM 4 vCPU. Linux container thông thường sẽ đọc `/proc/cpuinfo` và nhận nhầm 4 vCPU của máy chủ host.
- **Phân giải đúng qua CFS Quota**: Ứng dụng tự động quét `/proc/self/cgroup` để tìm đường dẫn cgroup thực sự của Pod trên Kubernetes/AKS (`/kubepods.slice/...`), đọc quota và period từ `cpu.max` (cgroup v2) hoặc `cpu.cfs_quota_us` (cgroup v1).
- **Tính toán động chính xác**: Bất kể bạn cấp `0.25`, `0.5`, `1.0`, `1.25`, `1.5`, hay `2.0` vCPU, ứng dụng đều nhận diện đúng số vCPU thực tế mà không cần cấu hình cứng.

### 2. Tích hợp HTTP Traffic Generator trực tiếp trên WebUI
- **Bắn tải HTTP sang Domain tùy chọn**: Nhập bất kỳ URL / FQDN nào (ví dụ FQDN do Azure Container Apps cấp). Hệ thống tự động ghi nhớ vào `localStorage`.
- **Tùy chỉnh Concurrency thủ công & linh hoạt**: Thanh trượt từ 1 đến 500+ concurrent requests với các nút chọn nhanh (1, 10, 20, 50, 100, 200, 500).
- **Điều chỉnh tải On-The-Fly**: Bạn có thể kéo thay đổi mức tải (ví dụ từ 10 lên 50 rồi 100) **ngay trong lúc đang bắn request** mà không cần dừng hay ngắt kết nối.
- **Mô phỏng chuẩn quy tắc ACA HTTP Scaling**: Giữ ổn định số lượng concurrent requests in-flight nhằm kích hoạt trực tiếp quy tắc `--scale-rule-http-concurrency` của Azure Container Apps.
- **Live Telemetry thời gian thực**: Hiển thị RPS (Requests/sec), Latency trung bình (ms), Active Concurrent Workers và bảng thống kê HTTP Status codes (2xx, 4xx, 5xx, Errors).

### 3. Tải nội bộ Container (CPU & RAM Stress)
- Tải CPU chính xác qua thuật toán duty-cycle spinning kết hợp goroutine theo chu kỳ 100ms.
- Tải RAM qua cấp phát và ghi đè từng trang bộ nhớ (4KB) để tăng chuẩn `WorkingSetBytes` (khớp với Azure Monitor).
- Xả RAM tức thì qua `runtime.GC()` và `debug.FreeOSMemory()`.

---

## 🚀 Hướng dẫn Chạy Thử Nghiệm

### Cách 1: Chạy trực tiếp từ máy Local (Dùng để Bắn Tải sang Azure)

```bash
cd uet-cloud-test
go run main.go
# Mở trình duyệt tại: http://localhost:8080
```
- Chuyển sang tab **"Bắn Tải HTTP (Domain / Azure Container Apps)"**.
- Nhập URL FQDN của Azure Container App.
- Chọn số lượng concurrent requests (ví dụ 10 để chạy 1 replica, kéo lên 50 để kích hoạt scale-out).
- Bấm **"Bắt đầu Bắn Tải"**.

---

### Cách 2: Chạy bằng k6 từ dòng lệnh (CLI Load Testing)

Ứng dụng đi kèm script k6 `k6-http-concurrency-test.js`.

#### Kịch bản 1: Ramp-up tự động theo tài liệu ACA (10 -> 50 -> 100 -> 200 VUs)
```bash
k6 run -e TARGET_URL="https://<your-app>.<region>.azurecontainerapps.io/api/status" k6-http-concurrency-test.js
```

#### Kịch bản 2: Bắn tải cố định thủ công (Custom Concurrency & Duration)
```bash
# Ví dụ: Bắn 50 concurrent requests trong 3 phút
k6 run \
  -e TARGET_URL="https://<your-app>.<region>.azurecontainerapps.io/api/status" \
  -e VUS=50 \
  -e DURATION=3m \
  k6-http-concurrency-test.js
```

---

### Cách 3: Build Docker Image để Deploy lên Azure Container Apps

```bash
# Build image
docker build -t uet-cloud-test:latest .

# Chạy thử nghiệm local với cgroups 1.25 vCPU và 2.5GB RAM
docker run -d --name uet-test -p 8080:8080 --cpus=1.25 --memory=2.5g uet-cloud-test:latest

# Kiểm tra limits nhận diện qua curl
curl -s http://localhost:8080/api/status | jq .limits
```

---

## 📡 REST API Tham chiếu

### 1. HTTP Traffic Generator (Bắn request ra ngoài)

| Endpoint | Method | Mô tả | Payload mẫu |
| :--- | :--- | :--- | :--- |
| `/api/traffic/start` | POST | Bắt đầu bắn request | `{"target_url":"https://...","concurrency":50,"duration_seconds":60}` |
| `/api/traffic/update` | POST | Thay đổi concurrency on-the-fly | `{"concurrency":100}` |
| `/api/traffic/stop` | POST | Dừng bắn request | `{}` |
| `/api/traffic/status` | GET | Lấy RPS, latency, worker, status | *Trả về JSON số liệu thời gian thực* |

### 2. Container Stress (Tải tài nguyên nội bộ)

| Endpoint | Method | Mô tả | Payload mẫu |
| :--- | :--- | :--- | :--- |
| `/api/load` | POST | Áp dụng tải CPU/RAM | `{"cpu_percent":50,"memory_percent":50,"duration_seconds":60}` |
| `/api/stop` | POST | Dừng tải và giải phóng RAM | `{}` |
| `/api/status` | GET | Thông tin cgroup và CPU/RAM thực tế | *Trả về JSON số liệu container* |
