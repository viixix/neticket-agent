package agent

import (
	"fmt"
	"math"
	"math/rand"
	"net/http"
	"time"
)

// -----------------------------------------------------------------------
// 페르소나별 파라미터 상수
// -----------------------------------------------------------------------

type personalityParams struct {
	patienceMin       time.Duration
	patienceMax       time.Duration
	pollIntervalSecs  float64 // 폴링 주기 μ (초) — 프론트엔드 refetchInterval 2s 기준
	pollJitterSecs    float64 // 폴링 주기 σ (초)
	stagnantThreshold int     // 이 횟수만큼 순서 변화 없으면 PanicMode
}

var personalityTable = map[PersonalityType]personalityParams{
	PersonalityHopeful: {
		patienceMin:       3 * time.Minute,
		patienceMax:       5 * time.Minute,
		pollIntervalSecs:  2.0,
		pollJitterSecs:    0.2,
		stagnantThreshold: 3,
	},
	PersonalityDoubtful: {
		patienceMin:       2 * time.Minute,
		patienceMax:       3 * time.Minute,
		pollIntervalSecs:  2.0,
		pollJitterSecs:    0.2,
		stagnantThreshold: 3,
	},
	PersonalityHopeless: {
		patienceMin:       1 * time.Minute,
		patienceMax:       2 * time.Minute,
		pollIntervalSecs:  2.0,
		pollJitterSecs:    0.2,
		stagnantThreshold: 2,
	},
}

// -----------------------------------------------------------------------
// Agent 구조체
// -----------------------------------------------------------------------

// Agent는 티켓팅 시뮬레이터의 가상 유저 한 명을 나타냅니다.
// 모든 필드는 이 에이전트 전용이며 다른 고루틴과 공유하지 않습니다.
type Agent struct {
	// --- 식별 및 설정 ---
	ID              int
	PersonalityType PersonalityType
	config          *Config
	client          *http.Client // 공유 Transport + 에이전트 전용 CookieJar

	// --- 상태 머신 ---
	CurrentState State

	// 대기 시작 시각과 인내심 한계. 초과 시 Aborted로 전이.
	PatienceLimit time.Duration
	startTime     time.Time

	// μ + σ*N(0,1) 으로 폴링 주기를 계산. 프론트엔드 refetchInterval(2s) 기준.
	BasePollInterval time.Duration // μ
	PollJitter       float64       // σ (초 단위)

	// [Lock Contention 방지] 전역 rand 대신 에이전트별 독립 인스턴스.
	// 50,000 고루틴이 전역 mutex를 공유하면 심각한 경합이 발생하므로
	// NewAgent()에서 고유 시드로 초기화합니다.
	rng *rand.Rand

	// --- 스트레스 가중 모드 (PanicMode) ---
	// 순번이 StagnantThreshold 회 연속 줄지 않으면 폴링 간격을 100~300ms로 단축해
	// 실제보다 공격적인 상한선 시나리오를 재현한다.
	StagnantCount     int
	StagnantThreshold int
	PanicMode         bool

	// --- 시뮬레이션 대상 (에이전트마다 다를 수 있음) ---
	// main.go에서 자동 발견 또는 CLI 값으로 주입됩니다.
	SessionID int
	BlockID   int

	// --- 런타임 상태 (메모리 최소화: 작은 스칼라값만 보유) ---
	lastPosition    int       // 직전 폴링에서 확인한 대기 순번
	waitingToken    string    // waiting-token 쿠키 값 (대기열 식별자 = userId)
	activeToken     string    // 대기열 통과 후 발급된 JWT
	conflictRetries int       // 이선좌(Conflict) 발생 시 재시도 횟수 (최대 3)
	selectedSeat    seatCoord // doSeatSelect → doReserve 로 선택 좌석 전달
	spoofedIP       string    // X-Forwarded-For 위조 IP (SpoofIP=true 시 사용)

	// --- 측정값 (고루틴 종료 후 main에서 집계) ---
	QueueLatency time.Duration // 대기열 진입 → 통과(position=0)까지 소요 시간

	// --- 공유 카운터 (모든 에이전트가 동일 인스턴스를 가리킴) ---
	counters *SharedCounters
}

// -----------------------------------------------------------------------
// 생성자
// -----------------------------------------------------------------------

// NewAgent는 페르소나 비율(33%/33%/34%)에 따라 에이전트를 생성합니다.
//
// sharedTransport는 pkg/util.NewTransport()로 만든 단일 인스턴스를 전달하고,
// CookieJar는 내부에서 에이전트 전용으로 생성합니다.
// sb.SessionID/BlockID 가 0이면 Config 기본값을 사용합니다.
func NewAgent(id int, cfg *Config, client *http.Client, sb SessionBlock, counters *SharedCounters) *Agent {
	// 전역 rand와 독립된 시드: 나노초 XOR ID로 고루틴 간 시드 중복 방지.
	seed := time.Now().UnixNano() ^ int64(id)
	rng := rand.New(rand.NewSource(seed)) //nolint:gosec // 보안 목적이 아닌 시뮬레이션용

	// SpoofIP용 가상 IP: 에이전트 생성 시 1회 생성 후 고정.
	// 1.0.0.0 ~ 223.255.255.254 범위 (예약 대역 제외하지 않으나 테스트 목적이므로 무관)
	spoofedIP := fmt.Sprintf("%d.%d.%d.%d",
		rng.Intn(223)+1,
		rng.Intn(256),
		rng.Intn(256),
		rng.Intn(254)+1,
	)

	personality := pickPersonality(rng)
	params := personalityTable[personality]

	// PatienceLimit: [min, max) 구간의 균등분포
	patienceRange := params.patienceMax - params.patienceMin
	patience := params.patienceMin + time.Duration(rng.Int63n(int64(patienceRange)))

	// SessionID/BlockID: 자동 발견 값 우선, 없으면 CLI 기본값 사용
	sessionID := sb.SessionID
	if sessionID == 0 {
		sessionID = cfg.SessionID
	}
	blockID := sb.BlockID
	if blockID == 0 {
		blockID = cfg.BlockID
	}

	return &Agent{
		ID:              id,
		PersonalityType: personality,
		config:          cfg,
		client:          client,

		SessionID: sessionID,
		BlockID:   blockID,

		CurrentState: StateIdle,

		PatienceLimit:     patience,
		BasePollInterval:     time.Duration(params.pollIntervalSecs * float64(time.Second)),
		PollJitter:       params.pollJitterSecs,
		StagnantThreshold: params.stagnantThreshold,

		rng:       rng,
		spoofedIP: spoofedIP,
		counters:  counters,
	}
}

// pickPersonality는 Hopeful 33% / Doubtful 33% / Hopeless 34% 비율로
// 페르소나를 선택합니다.
func pickPersonality(rng *rand.Rand) PersonalityType {
	n := rng.Intn(100)
	switch {
	case n < 33:
		return PersonalityHopeful
	case n < 66:
		return PersonalityDoubtful
	default:
		return PersonalityHopeless
	}
}

// -----------------------------------------------------------------------
// ThinkTime 계산 헬퍼
// -----------------------------------------------------------------------

const minPollInterval = 50 * time.Millisecond

// thinkTime은 Gaussian 분포로 폴링 간격을 계산합니다.
// 하한을 50ms로 클램핑하여 음수·0 값을 방지합니다.
func (a *Agent) pollInterval() time.Duration {
	µ := a.BasePollInterval.Seconds()
	jitter := a.rng.NormFloat64() * a.PollJitter
	secs := math.Max(minPollInterval.Seconds(), µ+jitter)
	return time.Duration(secs * float64(time.Second))
}

// panicThinkTime은 스트레스 가중 모드(PanicMode)의 폴링 간격을 반환합니다.
// 100~300ms 균등분포로 폴링을 가속해 실제보다 공격적인 부하를 재현합니다.
func (a *Agent) panicPollInterval() time.Duration {
	ms := 100 + a.rng.Intn(200)
	return time.Duration(ms) * time.Millisecond
}

// hesitateTime은 SeatSelect에서 좌석을 고르는 '망설임' 지연을 반환합니다.
// 0.5s~1.5s 균등분포.
func (a *Agent) hesitateTime() time.Duration {
	ms := 500 + a.rng.Intn(1000)
	return time.Duration(ms) * time.Millisecond
}

// -----------------------------------------------------------------------
// 샘플링 로그 헬퍼
// -----------------------------------------------------------------------

// shouldLog는 Config.LogEvery 간격으로 샘플링된 에이전트만 true를 반환합니다.
// LogEvery=1 이면 전체 에이전트 로그 출력, LogEvery=1000 이면 0.1%만 출력합니다.
func (a *Agent) shouldLog() bool {
	return a.config.LogEvery > 0 && a.ID%a.config.LogEvery == 0
}
