# Delivery Service (ระบบจัดส่งอาหาร)

บริการ Delivery ของโครงงานตาม `Food_Ordering_API_Spec_Go_Microservices.pdf` เขียนด้วย Go ใช้พอร์ต `8084` และฐานข้อมูล PostgreSQL ชื่อ `delivery_db` มี API สาธารณะ 8 เส้นทางตามหัวข้อ 7 ของเอกสาร ไม่มีหน้าเว็บ และไม่จำเป็นต้องใช้ ngrok เมื่อลองในเครื่องเดียวกัน

## วิธีรันในเครื่อง

ต้องมี Go 1.26 ขึ้นไปและ PostgreSQL 16 ขึ้นไป เปิด Terminal ที่โฟลเดอร์โปรเจกต์นี้ แล้วทำตามลำดับ:

1. เปิดฐานข้อมูลด้วย `docker compose up -d delivery-db` หรือใช้ PostgreSQL ที่เตรียมไว้เอง คำสั่งนี้เปิด **เฉพาะฐานข้อมูล** ไม่ได้เปิด Go Service
2. คัดลอก `.env.example` เป็น `.env` แล้วกำหนด `JWT_SECRET` และ `INTERNAL_API_KEY` ให้ตรงกับค่าที่ตกลงใช้ร่วมกับทีม เก็บ `.env` ไว้เฉพาะเครื่อง ห้ามพุชขึ้น Git
3. โหลดค่าใน `.env` และรัน Service ด้วย PowerShell:

   ```powershell
   Get-Content .env | ForEach-Object {
     if ($_ -match '^([^#=]+)=(.*)$') { [Environment]::SetEnvironmentVariable($matches[1], $matches[2], 'Process') }
   }
   go run -buildvcs=false ./cmd/server
   ```

4. ตรวจ `http://localhost:8084/healthz` ระบบจะปรับโครงสร้างฐานข้อมูล (migration) ตอนเริ่มทำงาน
5. รันทดสอบด้วย `go test ./...`

เมื่อต้องเชื่อมกับเพื่อน ให้ตั้ง `ORDER_SERVICE_URL`, `USER_SERVICE_URL` และ `RESTAURANT_SERVICE_URL` เป็น URL ต้นทางของแต่ละ Service เช่น `https://example.ngrok-free.dev` **ไม่ต้องต่อ `/api/v1`** เพราะโค้ดต่อ path ให้เอง ใช้ได้ทั้ง URL ใน LAN, ngrok หรือเซิร์ฟเวอร์ที่เผยแพร่แล้ว โดยไม่ต้องแก้ source code

หากเพื่อนต้องเรียก Delivery Service ข้ามเครื่อง ต้องรัน Go Service ให้ตอบที่พอร์ต `8084` ก่อน แล้วจึงเปิด ngrok แยกอีก Terminal ด้วย `ngrok http 8084` และส่ง URL ที่ ngrok แสดงให้เพื่อน ต้องเปิดทั้ง Service และ tunnel ค้างไว้ระหว่างทดสอบ

## API และสิทธิ์การใช้งาน

| Method | Path | ผู้มีสิทธิ์ |
| --- | --- | --- |
| POST | `/api/v1/deliveries` | `restaurant_owner` ของร้านนั้น หรือ `admin` |
| GET | `/api/v1/deliveries` | `rider` เห็นเฉพาะงานตัวเอง หรือ `admin` |
| GET | `/api/v1/deliveries/{id}` | ลูกค้า เจ้าของร้าน ไรเดอร์ที่เกี่ยวข้อง หรือ `admin` |
| PUT | `/api/v1/deliveries/{id}/assign` | `admin` |
| PUT | `/api/v1/deliveries/{id}/status` | ไรเดอร์ที่รับงาน หรือ `admin` |
| GET | `/api/v1/deliveries/{id}/track` | ลูกค้า ไรเดอร์ที่เกี่ยวข้อง หรือ `admin` |
| GET | `/api/v1/riders/{id}/deliveries` | ไรเดอร์เจ้าของรายการ หรือ `admin` |
| GET | `/api/v1/admin/dashboard` | `admin` |

API ที่มีการป้องกันต้องส่ง `Authorization: Bearer <JWT>` โดย JWT ใช้ HS256, secret ร่วมกัน และมี `sub` เป็น UUID v4, `role`, `exp` ส่วน POST/PUT ต้องส่ง `Content-Type: application/json` การสร้างงานรับ `Idempotency-Key` เป็นตัวเลือกเพื่อระบุคำขอซ้ำ และ `order_id` ต้องไม่ซ้ำ แม้ไม่ส่ง key รายการแบบแบ่งหน้ารับ `page` (ค่าเริ่มต้น 1), `limit` (ค่าเริ่มต้น 20 สูงสุด 100) และ `status` คำตอบสำเร็จมี `success` และ `data`; คำตอบผิดพลาดมี `success`, `error`, `message` และ `details`

ตัวอย่างคำขอสร้างงานจัดส่ง:

```http
POST http://localhost:8084/api/v1/deliveries
Authorization: Bearer <restaurant-owner-token>
Content-Type: application/json
Idempotency-Key: order-44444444-4444-4444-8444-444444444444

{"order_id":"44444444-4444-4444-8444-444444444444","pickup_address":"123 ถนนสุขุมวิท","dropoff_address":"88/8 ถนนพระราม 4"}
```

ก่อนสร้างงาน ระบบตรวจว่า Order มีสถานะ `ready` และยอมให้มีงานจัดส่งได้หนึ่งงานต่อหนึ่ง Order การมอบหมายงานเปลี่ยน `waiting_rider` เป็น `assigned` จากนั้นต้องเปลี่ยนสถานะตามลำดับ `assigned -> picked_up -> on_the_way -> delivered` โดย `failed` ใช้จบงานที่ยังดำเนินอยู่ได้ การอ่านรายการ/รายละเอียดตรวจสิทธิ์จากข้อมูลเจ้าของที่บันทึกไว้ตอนสร้างงาน

พิกัดใน `/track` เป็น `null` จนกว่าจะมีการอัปเดต GPS เอกสารหลักกำหนด API อ่านพิกัด แต่ไม่ได้กำหนด API เขียนพิกัด โปรเจกต์นี้จึงเพิ่ม API ภายใน `PUT /api/v1/internal/deliveries/{id}/location` สำหรับ Service-to-Service ต้องส่ง `X-Internal-Key` และ JSON `{"lat":13.7563,"lng":100.5018}` อัปเดตได้เฉพาะงานที่ยังดำเนินอยู่ จากนั้นเรียก `/track` เพื่ออ่านพิกัดล่าสุด

## ข้อตกลงสำหรับเชื่อมกับ Service ของเพื่อน

ตัวอย่าง `GET /api/v1/orders/{id}` และ `GET /api/v1/restaurants/{id}` ใน PDF ไม่แสดงบาง field ที่จำเป็นต่อการตรวจเจ้าของ สำหรับการสร้างงาน Order Service ต้องส่ง `id`, `status`, `customer_id` และ `restaurant_id` ใน `data` โค้ด Order Service ที่เคยตรวจ ณ commit `6eeef585373e52ebc1daf39b67864977518d69a8` มีข้อมูล Order เหล่านี้แล้ว ส่วน Restaurant Service ต้องส่ง `owner_id` ใน `data` ของ `GET /api/v1/restaurants/{id}` หากข้อมูลไม่ครบ Delivery Service จะตอบ 503 แทนการเดาเจ้าของ

เมื่อ Admin มอบหมายงาน ระบบตรวจ `rider_id` ผ่าน `GET /api/v1/users/{id}` ของ Auth & User Service ซึ่งต้องส่ง `role: rider` และ `status: active` โค้ด Auth Service ที่ตรวจ ณ commit `cc78fa36eb5d705e931438423346f8c79f847a23` มี endpoint และ field ดังกล่าว โดย endpoint นี้รับ token ของ `admin`

Dashboard เรียก API รายการของ User, Restaurant และ Order พร้อมกันเพื่ออ่าน `meta.total_items` แล้วนับ Delivery จากฐานข้อมูลของตัวเอง ถ้าต้องการให้ `total_restaurants` ครอบคลุมทุกสถานะ Restaurant Service ต้องให้ Admin เห็นร้านที่ยัง `pending` ด้วย เมื่อส่ง `date_from`/`date_to` ระบบอ่าน `created_at` ของแต่ละรายการเพื่อกรองวัน ดังนั้นทั้งสาม Service ต้องส่ง field นี้ หาก Service ใดล่มหรือข้อมูลไม่ครบ Dashboard ตอบ 503 ทั้งชุด ไม่ส่งยอดรวมที่ขาดบางส่วน Dashboard เป็น API JSON ไม่ใช่หน้าเว็บ

Order Service ที่ตรวจใน commit ข้างต้นส่ง `count` แต่ยังไม่มี `meta.total_items` ใน `GET /api/v1/orders` จึงทำให้ Dashboard ตอบ 503 จนกว่า Order Service จะเพิ่ม metadata การแบ่งหน้าตามข้อตกลง การดูรายละเอียด Order ใช้กับการสร้าง Delivery ได้ ส่วน Restaurant Service ยังต้องทดสอบเชื่อมจริง

Delivery Service ส่ง Bearer token ของผู้เรียกและ `X-Internal-Key` ไปยัง Service ของเพื่อน Service ปลายทางต้องรับ header เหล่านี้ตาม endpoint ที่เกี่ยวข้อง ใช้ `JWT_SECRET` เดียวกันทุก Service และตกลงค่า `INTERNAL_API_KEY` ร่วมกัน Delivery Service เข้าถึงฐานข้อมูลของตัวเอง (`delivery_db`) เท่านั้น ID ที่อ้างถึงข้อมูลของ Service อื่นไม่มี foreign key ข้ามฐานข้อมูล

## ความสอดคล้องกับเอกสารออกแบบฐานข้อมูลฉบับใหม่

เอกสารฐานข้อมูลฉบับใหม่เพิ่ม `assigned_at` และ `picked_up_at` ในตาราง `deliveries` โค้ดสร้างหรือเพิ่มสองคอลัมน์นี้ให้ฐานข้อมูลเดิม และบันทึกเวลาเมื่อเปลี่ยนสถานะตามลำดับ `order_id` ยังคงห้ามซ้ำ ส่วน `rider_id` เป็น `NULL` ได้ก่อนมอบหมายงาน ID ของ Service อื่นไม่มี foreign key ข้ามฐานข้อมูล เวลาใช้ `TIMESTAMPTZ` เพื่อรักษาค่า UTC/RFC 3339 ของ API แม้เอกสารออกแบบจะระบุเป็น `TIMESTAMP` ทั่วไป

ตารางยังมี `customer_id`, `restaurant_id`, `restaurant_owner_id` เป็นข้อมูล snapshot สำหรับตรวจสิทธิ์ และ `lat`, `lng` สำหรับติดตามพิกัด รวมถึง `idempotency_key`, `idempotency_actor_id`, `request_hash` สำหรับจัดการคำขอซ้ำ คอลัมน์เหล่านี้ไม่ได้อยู่ในตารางของเอกสารออกแบบ แต่รองรับข้อกำหนด API ฐานข้อมูลใหม่มีเพียงตาราง `deliveries` หากเคยใช้ schema รุ่นเก่าที่มีตาราง `idempotency_keys` ตอนเริ่มระบบจะคัดลอกข้อมูลที่จำเป็นมายัง `deliveries` โดยไม่ลบตารางเก่า ควรสำรองฐานข้อมูลและตรวจสอบก่อนลบตารางเก่า ทีมควรตกลงว่าเอกสารออกแบบระบุคอลัมน์ขั้นต่ำหรือห้ามมีคอลัมน์เพิ่มเติมก่อนส่งงาน
