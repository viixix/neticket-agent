# neticket-agent 사용 가이드

Neticket 티켓팅 시스템에 가상 유저를 대량으로 투입해 부하를 시뮬레이션하는 CLI 도구.

## 빌드

```bash
go build -o agent ./cmd/agent
```

## 기본 실행

```bash
# 운영 서버에 50,000명 투입 (기본값)
./agent

# 로컬 서버에 1,000명 투입
./agent \
  --agents 1000 \
  --queue-url http://localhost:3003/api \
  --booking-url http://localhost:3002/api \
  --api-url http://localhost:3001/api
```

## CLI 플래그

| 플래그 | 기본값 | 설명 |
|---|---|---|
| `--agents` | `50000` | 생성할 가상 유저 수 |
| `--queue-url` | `https://queue.neticket.site/api` | queue 서비스 Base URL |
| `--booking-url` | `https://booking.neticket.site/api` | booking 서비스 Base URL |
| `--api-url` | `https://show.neticket.site/api` | show 서비스 Base URL (자동 발견용) |
| `--auto-discover` | `true` | 활성/예정 티켓팅 세션·블록 자동 조회 |
| `--session-id` | `0` | 수동 지정 시 회차 ID (`--auto-discover=false`와 함께) |
| `--block-id` | `0` | 수동 지정 시 구역 ID (`--auto-discover=false`와 함께) |
| `--skip-captcha` | `true` | 캡차 단계 스킵 (서버 bypass 설정 필요) |
| `--log-every` | `1000` | 로그 샘플링 간격 (ID % N == 0인 에이전트만 출력) |

## 세션/블록 지정 방식

### 자동 발견 (기본)

`--auto-discover=true`(기본값) 시 `--api-url`에서 공연 목록을 조회해 다음 기준으로 대상을 선택한다.

- **진행 중**: `ticketing_date` ≤ 현재 시각이고 시작 후 60분 이내
- **예정**: 가장 가까운 `ticketing_date` 기준 ±5분 이내

발견된 (세션, 블록) 쌍이 여러 개면 에이전트에 순환 배정한다.

### 수동 지정

```bash
./agent --auto-discover=false --session-id 12 --block-id 3
```

## 에이전트 행동 모델

에이전트는 3가지 페르소나로 분류되어 실제 유저 행동을 모사한다.

| 페르소나 | 비율 | 인내심 | 특징 |
|---|---|---|---|
| Standard | 33% | 3~5분 | 평균적인 유저 |
| Urgent | 33% | 2~3분 | 빠르게 시도, 짧게 기다림 |
| Quitter | 34% | 1~2분 | 인내심 한계 도달 시 이탈 |

대기열에서 순위가 오랫동안 변하지 않으면 **PanicMode**가 활성화되어 폴링 간격이 짧아진다.

## 시스템 요건 (대규모 테스트)

에이전트 수만큼 TCP 연결이 필요하므로 다음을 미리 설정한다.

### Linux

```bash
# 현재 세션에만 적용
ulimit -n 100000

# 영구 적용 (/etc/security/limits.conf)
* soft nofile 100000
* hard nofile 100000
```

에이전트 실행 시 자동으로 `RLIMIT_NOFILE` 상향을 시도하며, 실패해도 경고만 출력하고 계속 실행한다.

### Windows

```powershell
netsh int ipv4 set dynamicport tcp start=1025 num=64511
netsh int ipv6 set dynamicport tcp start=1025 num=64511
```

## 단일 머신 부하 테스트 (IP 스푸핑)

단일 머신에서 테스트하면 모든 에이전트가 같은 출발지 IP를 공유하므로, nginx의 per-IP rate limit이 실제 운영 환경과 다르게 작동한다.  
`--spoof-ip` 플래그와 `nginx.loadtest.conf`를 함께 사용하면 에이전트마다 다른 IP로 인식되어 실제 분산 트래픽 조건을 재현할 수 있다.

### 절차

```bash
# 1. 서버: loadtest conf로 교체 (X-Forwarded-For 기준 rate limit)
docker cp queue/nginx.loadtest.conf <nginx_container>:/etc/nginx/nginx.conf
docker exec <nginx_container> nginx -s reload

# 2. 에이전트 실행 (--spoof-ip로 에이전트별 랜덤 IP 헤더 전송)
./agent --agents <에이전트 수> --spoof-ip

# 3. 테스트 후 운영 conf 복구
docker cp queue/nginx.conf <nginx_container>:/etc/nginx/nginx.conf
docker exec <nginx_container> nginx -s reload
```

> `nginx.loadtest.conf`는 `neticket` 저장소의 `queue/nginx.loadtest.conf`에 위치한다.

## 종료

`Ctrl+C` 또는 `SIGTERM` 수신 시 새 에이전트 생성을 중단하고 실행 중인 고루틴이 모두 완료될 때까지 대기한다.

```
[main] 시뮬레이션 완료 elapsed=2m34s Done=38421 Aborted=11579
```

## 캡차 설정

기본값 `--skip-captcha=true`이므로 서버 측 bypass가 활성화된 환경에서만 정상 동작한다.  
실제 캡차 해독이 필요한 경우 `--skip-captcha=false`로 설정하되, SVG 이미지 기반이므로 자동 해독은 불가능하다.
