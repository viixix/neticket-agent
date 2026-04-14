package agent

// Config는 에이전트 풀 전체가 공유하는 불변 설정입니다.
// 각 에이전트는 포인터로 참조하므로 메모리를 한 번만 차지합니다.
type Config struct {
	// 대기열 서버 (queue, 기본 포트 3003)
	QueueURL string

	// 예약 서버 (booking, 기본 포트 3002)
	BookingURL string

	// 시뮬레이션 대상 회차 ID
	SessionID int

	// 시뮬레이션 대상 구역 ID
	BlockID int

	// true면 캡차 단계를 건너뜁니다.
	// SVG 이미지 기반 캡차는 자동 해독이 불가능하므로
	// 부하 테스트 시에는 서버 측 bypass와 함께 활성화하세요.
	SkipCaptcha bool

	// 생성할 에이전트(고루틴) 수
	TotalAgents int

	// 로그 샘플링 간격: ID % LogEvery == 0 인 에이전트만 상태 로그를 출력합니다.
	// 기본값 1000 → 50,000명 중 약 50개만 출력.
	// 소규모 테스트 시 1로 설정하면 전체 에이전트 로그를 볼 수 있습니다.
	LogEvery int

	// API 서버 Base URL (자동 발견 시 사용, show 포트 3001)
	APIURL string

	// AutoDiscover: true면 APIURL에서 활성/예정 세션+블록을 자동 조회합니다.
	// false면 SessionID/BlockID CLI 값을 전체 에이전트에 동일하게 사용합니다.
	AutoDiscover bool
}

// DefaultConfig는 neticket.site 운영 환경을 기본값으로 반환합니다.
// 로컬 테스트 시 --queue-url, --booking-url, --api-url 플래그로 덮어쓸 수 있습니다.
func DefaultConfig() *Config {
	return &Config{
		QueueURL:     "https://queue.neticket.site/api",
		BookingURL:    "https://booking.neticket.site/api",
		SessionID:    0, // AutoDiscover=true 시 자동 설정됨
		BlockID:      0, // AutoDiscover=true 시 자동 설정됨
		SkipCaptcha:  true,
		TotalAgents:  50000,
		LogEvery:     1000,
		APIURL:        "https://show.neticket.site/api",
		AutoDiscover: true,
	}
}
