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

// PersonalityType은 티켓팅 대기 중 유저의 이탈 성향을 나타냅니다.
//
// 근거: NOL티켓·멜론티켓·티켓링크·예스24 등 실제 서비스의 좌석 선점 후
// 결제 제한 시간이 약 5~10분입니다. 유저는 이 시간 내에 선점 포기로
// 풀리는 좌석을 기대하며 대기하므로, Hopeful의 상한을 5분으로 설정하고
// 이탈 성향에 따라 나머지 두 구간을 상대적으로 조정했습니다.
type PersonalityType int

const (
	// PersonalityHopeful: 결제 제한 시간(~5분)까지 희망을 갖고 대기. PatienceLimit 3~5분.
	PersonalityHopeful PersonalityType = iota
	// PersonalityDoubtful: 중반 이전에 희망을 잃고 이탈. PatienceLimit 2~3분.
	PersonalityDoubtful
	// PersonalityHopeless: 초반 폴링 몇 회 안에 포기. PatienceLimit 1~2분.
	PersonalityHopeless
)

func (p PersonalityType) String() string {
	switch p {
	case PersonalityHopeful:
		return "Hopeful"
	case PersonalityDoubtful:
		return "Doubtful"
	case PersonalityHopeless:
		return "Hopeless"
	default:
		return fmt.Sprintf("Unknown(%d)", int(p))
	}
}
