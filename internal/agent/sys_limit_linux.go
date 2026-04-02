//go:build linux

package agent

import (
	"fmt"
	"syscall"
)

const targetFDLimit uint64 = 100_000

// ApplySystemLimits는 프로세스의 오픈 파일 디스크립터 한계를
// 5만 개 동시 커넥션이 가능한 수준으로 올립니다.
//
// Linux 기본값은 보통 1024(soft) / 1048576(hard)입니다.
// 5만 개 고루틴이 각각 소켓을 열면 FD가 빠르게 소진되므로
// 프로그램 시작 시 한 번 호출해야 합니다.
//
// 권한 부족(EPERM)이 발생하면 /etc/security/limits.conf 또는
// systemd의 LimitNOFILE을 통해 hard limit을 먼저 올려야 합니다.
func ApplySystemLimits() error {
	var current syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &current); err != nil {
		return fmt.Errorf("getrlimit 실패: %w", err)
	}

	// hard limit이 목표치보다 낮으면 경고만 남기고 최대한 올립니다.
	newMax := current.Max
	if newMax < targetFDLimit {
		fmt.Printf("[WARN] hard limit(%d)이 목표치(%d)보다 낮습니다. hard limit까지만 올립니다.\n",
			current.Max, targetFDLimit)
	} else {
		newMax = targetFDLimit
	}

	desired := syscall.Rlimit{Cur: newMax, Max: current.Max}
	if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &desired); err != nil {
		return fmt.Errorf("setrlimit(RLIMIT_NOFILE, %d) 실패: %w", newMax, err)
	}

	fmt.Printf("[INFO] RLIMIT_NOFILE 설정 완료: soft=%d, hard=%d\n", desired.Cur, desired.Max)
	return nil
}
