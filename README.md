# neticket-agent

Neticket 티켓팅 시스템의 가상 유저를 대량으로 투입해 대기열·예약 서버의 부하를 시뮬레이션하는 CLI 도구.

## 개요

실제 티켓팅 유저의 이탈 성향을 3가지 페르소나로 모델링하고, 각 에이전트가 독립된 고루틴으로 동작해 대기열 진입 → 좌석 선택 → 예약 확정까지의 전 흐름을 재현한다.

## 에이전트 동작 모델

### 상태 머신

```
         POST /queue/entries
Idle ──────────────────────── 실패 ──→ Aborted
              ↓
         Queueing  ←── GET /queue/entries/me (2s 폴링)
              │
    PatienceLimit 초과 ──→ Aborted
              │ position=0 + JWT
         SeatSelect ── 빈 좌석 없음 ──→ Aborted
              │ 망설임 지연 (0.5~1.5s)
         Reserving ── 이미 선택된 좌석(최대 3회 재시도 후) ──→ Aborted
              │ HTTP 201
            Done
```

<details>
<summary>이미 선택된 좌석 처리 구현 위치</summary>

예약 시도 시 다른 유저가 먼저 동일 좌석을 선점한 경우 서버가 HTTP 400을 반환한다.

- **서버** (`core/booking/src/reservation/reservation.service.ts`): Redis atomic reservation Lua 스크립트에서 좌석 선점 충돌 감지 → `SEATS_ALREADY_RESERVED` (HTTP 400)
- **에이전트** (`internal/agent/run.go`): HTTP 400 수신 시 SeatSelect로 회귀해 다른 좌석 재선택. 최대 3회 초과 시 Aborted.

</details>

### 페르소나

실제 티켓팅 서비스(NOL티켓·멜론티켓·티켓링크·YES24)의 좌석 선점 후 결제 제한 시간(5~10분)을 기준으로 이탈 성향을 구분한다.

| 페르소나 | 비율 | 인내심(PatienceLimit) | 특징 |
|---|---|---|---|
| Hopeful | 33% | 3~5분 | 결제 제한 시간까지 좌석 해제를 기대하며 대기 |
| Doubtful | 33% | 2~3분 | 희망을 잃고 중반 이전에 이탈 |
| Hopeless | 34% | 1~2분 | 초반 폴링 몇 회 안에 포기하고 이탈 |

### 폴링 동작

기본값은 프론트엔드 `refetchInterval`(2s)과 동일한 고정 주기를 사용한다.  
`--adaptive-polling` 활성화 시 대기 순번에 따라 주기를 동적으로 조정해 서버 부하를 절감한다.

| 조건 | 폴링 주기 | 근거 |
|---|---|---|
| position > 5 × capacity | 10s | 입장까지 5분 이상 |
| position > 1 × capacity | 5s | 입장까지 1분 이상 |
| position > capacity / 10 | 2s | 프론트엔드 기준값 |
| position ≤ capacity / 10 | 1s | 입장 직전, 응답성 최대화 |

capacity 기준: 서버 `worker.max_capacity`(기본 1000), `active_ttl` 60s → 처리율 ≈ 17명/초에서 역산.

## 빌드

```bash
go build -o agent ./cmd/agent
```

## 빠른 시작

```bash
# 운영 서버에 50,000명 투입 (기본값)
./agent

# 로컬 서버에 1,000명 투입
./agent \
  --agents 1000 \
  --queue-url http://localhost:3003/api \
  --booking-url http://localhost:3002/api \
  --api-url http://localhost:3001/api

# adaptive polling 활성화 (서버 capacity가 5000으로 늘어난 경우)
./agent --adaptive-polling --queue-capacity 5000
```

## 출력 해석

```
[진행] Done=352(H=120 D=115 HL=117) Aborted=1436(H=480 D=476 HL=480) Running=1212 / Total=3000 | queue p50=8.2s p95=22.1s | 500=0 502=3 503=12 net=0
```

| 항목 | 설명 |
|---|---|
| `Done` / `Aborted` | 예약 성공 / 이탈 (인내심 초과 또는 오류) |
| `H` / `D` / `HL` | Hopeful / Doubtful / Hopeless 페르소나별 집계 |
| `queue p50` / `p95` | 대기열 진입 → 통과까지 소요 시간 백분위수 |
| `500` / `502` / `503` / `net` | HTTP 에러 및 네트워크 오류 누적 횟수 |

시뮬레이션 종료 시 최종 요약이 출력된다.

```
[결과] Done=1564 / Aborted=1436 / Total=3000
[페르소나] Hopeful Done=534 Aborted=465 | Doubtful Done=521 Aborted=479 | Hopeless Done=509 Aborted=492
[대기열 latency] 샘플=1564 p50=8.4s p95=23.7s p99=31.2s
[HTTP 에러] 500=0 502=5 503=24 net=2
```

## 주요 플래그

| 플래그 | 기본값 | 설명 |
|---|---|---|
| `--agents` | `50000` | 생성할 가상 유저 수 |
| `--queue-url` | `https://queue.neticket.site/api` | queue 서비스 Base URL |
| `--booking-url` | `https://booking.neticket.site/api` | booking 서비스 Base URL |
| `--api-url` | `https://show.neticket.site/api` | show 서비스 Base URL (자동 발견용) |
| `--auto-discover` | `true` | 활성/예정 티켓팅 세션·블록 자동 조회 |
| `--adaptive-polling` | `false` | 대기 순번 기반 adaptive polling 활성화 |
| `--queue-capacity` | `1000` | 서버 `worker.max_capacity` (adaptive polling 구간 계산용) |
| `--spoof-ip` | `false` | 에이전트별 랜덤 X-Forwarded-For 헤더 (단일 머신 rate limit 우회) |
| `--max-duration` | 무제한 | 테스트 최대 실행 시간 (예: `5m`) |
| `--log-every` | `1000` | 로그 샘플링 간격 (1=전체 출력) |

자세한 사용법(시스템 설정, IP 스푸핑, 캡차 설정 등)은 [USAGE.md](USAGE.md)를 참조.
