package agent

import (
	"math"
	"math/rand"
	"net/http"
	"time"
)

// -----------------------------------------------------------------------
// 페르소나별 파라미터 상수
// Yu et al. "Delay Information in Virtual Queues" 기반 PatienceLimit,
// Flash Crowd Attacks 연구 기반 Gaussian Jitter (μ, σ).
// -----------------------------------------------------------------------

type personalityParams struct {
	patienceMin       time.Duration
	patienceMax       time.Duration
	thinkMeanSecs     float64 // μ (초)
	thinkStddevSecs   float64 // σ (초)
	stagnantThreshold int     // Maister: 이 횟수만큼 순서 변화 없으면 PanicMode
}

var personalityTable = map[PersonalityType]personalityParams{
	PersonalityStandard: {
		patienceMin:       3 * time.Minute,
		patienceMax:       5 * time.Minute,
		thinkMeanSecs:     1.6,
		thinkStddevSecs:   0.4,
		stagnantThreshold: 3,
	},
	PersonalityUrgent: {
		patienceMin:       2 * time.Minute,
		patienceMax:       3 * time.Minute,
		thinkMeanSecs:     1.0,
		thinkStddevSecs:   0.3,
		stagnantThreshold: 3,
	},
	PersonalityQuitter: {
		patienceMin:       1 * time.Minute,
		patienceMax:       2 * time.Minute,
		thinkMeanSecs:     1.6,
		thinkStddevSecs:   0.4,
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

	// --- Yu et al.: PatienceLimit 모델 ---
	// 대기 시작 시각과 인내심 한계. 초과 시 Aborted로 전이.
	PatienceLimit time.Duration
	startTime     time.Time

	// --- Flash Crowd Attacks: Gaussian Jitter ---
	// μ + σ*N(0,1) 으로 ThinkTime을 계산.
	BaseThinkTime time.Duration // μ
	JitterRange   float64       // σ (초 단위)

	// [Lock Contention 방지] 전역 rand 대신 에이전트별 독립 인스턴스.
	// 50,000 고루틴이 전역 mutex를 공유하면 심각한 경합이 발생하므로
	// NewAgent()에서 고유 시드로 초기화합니다.
	rng *rand.Rand

	// --- Maister: 대기열 심리학 PanicMode ---
	// 순서가 StagnantThreshold 회 연속 줄지 않으면 PanicMode 돌입.
	// PanicMode에서는 ThinkTime 대신 100~300ms 연타 간격을 사용합니다.
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
}

// -----------------------------------------------------------------------
// 생성자
// -----------------------------------------------------------------------

// NewAgent는 페르소나 비율(33%/33%/34%)에 따라 에이전트를 생성합니다.
//
// sharedTransport는 pkg/util.NewTransport()로 만든 단일 인스턴스를 전달하고,
// CookieJar는 내부에서 에이전트 전용으로 생성합니다.
// sb.SessionID/BlockID 가 0이면 Config 기본값을 사용합니다.
func NewAgent(id int, cfg *Config, client *http.Client, sb SessionBlock) *Agent {
	// 전역 rand와 독립된 시드: 나노초 XOR ID로 고루틴 간 시드 중복 방지.
	seed := time.Now().UnixNano() ^ int64(id)
	rng := rand.New(rand.NewSource(seed)) //nolint:gosec // 보안 목적이 아닌 시뮬레이션용

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
		BaseThinkTime:     time.Duration(params.thinkMeanSecs * float64(time.Second)),
		JitterRange:       params.thinkStddevSecs,
		StagnantThreshold: params.stagnantThreshold,

		rng: rng,
	}
}

// pickPersonality는 Standard 33% / Urgent 33% / Quitter 34% 비율로
// 페르소나를 선택합니다.
func pickPersonality(rng *rand.Rand) PersonalityType {
	n := rng.Intn(100)
	switch {
	case n < 33:
		return PersonalityStandard
	case n < 66:
		return PersonalityUrgent
	default:
		return PersonalityQuitter
	}
}

// -----------------------------------------------------------------------
// ThinkTime 계산 헬퍼
// -----------------------------------------------------------------------

const minThinkTime = 50 * time.Millisecond

// thinkTime은 Flash Crowd 연구 기반 Gaussian 분포로 대기 시간을 계산합니다.
// 하한을 50ms로 클램핑하여 음수·0 값을 방지합니다.
func (a *Agent) thinkTime() time.Duration {
	µ := a.BaseThinkTime.Seconds()
	jitter := a.rng.NormFloat64() * a.JitterRange
	secs := math.Max(minThinkTime.Seconds(), µ+jitter)
	return time.Duration(secs * float64(time.Second))
}

// panicThinkTime은 PanicMode(광클) 상태의 연타 간격을 반환합니다.
// 100~300ms 균등분포: 실제 유저의 손가락 연타 속도를 모델링합니다.
func (a *Agent) panicThinkTime() time.Duration {
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
