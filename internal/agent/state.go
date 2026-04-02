package agent

import "fmt"

// State는 에이전트의 생애 주기 상태를 나타냅니다.
type State int

const (
	StateIdle       State = iota // 초기 상태
	StateQueueing                // 대기열 진입 및 폴링 중
	StateSeatSelect              // 좌석 조회 및 선택 중
	StateReserving               // 예약 확정 시도 중
	StateDone                    // 예약 성공
	StateAborted                 // 인내심 한계 초과 또는 최대 재시도 도달로 이탈
)

func (s State) String() string {
	switch s {
	case StateIdle:
		return "Idle"
	case StateQueueing:
		return "Queueing"
	case StateSeatSelect:
		return "SeatSelect"
	case StateReserving:
		return "Reserving"
	case StateDone:
		return "Done"
	case StateAborted:
		return "Aborted"
	default:
		return fmt.Sprintf("Unknown(%d)", int(s))
	}
}

// PersonalityType은 Yu et al. / Maister 연구 기반의 유저 행동 유형입니다.
type PersonalityType int

const (
	// PersonalityStandard: 평균적인 유저. PatienceLimit 3~5분, StagnantThreshold 3.
	PersonalityStandard PersonalityType = iota
	// PersonalityUrgent: 조급한 유저. PatienceLimit 2~3분, BaseThinkTime 짧음.
	PersonalityUrgent
	// PersonalityQuitter: 쉽게 포기하는 유저. PatienceLimit 1~2분, StagnantThreshold 2.
	PersonalityQuitter
)

func (p PersonalityType) String() string {
	switch p {
	case PersonalityStandard:
		return "Standard"
	case PersonalityUrgent:
		return "Urgent"
	case PersonalityQuitter:
		return "Quitter"
	default:
		return fmt.Sprintf("Unknown(%d)", int(p))
	}
}
