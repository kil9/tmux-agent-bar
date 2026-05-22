# PLAN.md

`tmux-agent-bar` 의 작업 계획.

## 현재 상태 (Snapshot)

- 표시 상태: `🚨` (error) > `💬` (waiting) > `⏸` (planning) > `🤖` (thinking) > `✅` (done) > idle
- Claude Code hook 4종(`PreToolUse`, `Notification`, `Stop`, `SubagentStop`)을 통해 상태 파일을 `/tmp/tmux-agent-bar/<key>` 에 기록한다.
- 1초 주기 `runStatus` 가 각 윈도우의 pane 상태를 집계해 윈도우 이름 앞에 이모지를 삽입한다.

## 다음 할 일

### 1. Claude 가 백그라운드 작업을 띄워두고 대기 중일 때 별도 상태(⏳) 표시 ✅ 완료 (2026-05-22)

**배경**: 현재 Claude Code 가 Monitor / `Bash run_in_background` / shell 등을 띄워두고 외부 이벤트를 기다리는 동안에도 `Stop` hook 이 호출되어 `✅`(완료) 로 표시된다. 실제로는 "끝나지 않고 깨어날 신호를 기다리는 중" 이라 시각적으로 완료처럼 보이는 것이 어색하다. 백그라운드 대기 상태를 별도 시각 신호로 분리한다.

**목표**:
- 새 상태(가칭 `bg_waiting`, 최종 명명은 구현 단계에서 확정)를 추가하고 `⏳` 이모지로 표시한다.
- 우선순위: `🚨 > 💬 > ⏸ > 🤖 > ⏳ > ✅ > idle` (🤖과 ✅ 사이).
- 감지: `Stop` hook 시점에 `TMUX_PANE` 의 `pane_pid` 하위 자식 프로세스 트리를 검사해 살아있는 자식이 있으면 `done` 대신 `bg_waiting` 으로 기록한다.
- 보강: `runStatus` 렌더링 단계에서 `bg_waiting` 상태인 pane 의 자식이 모두 사라졌으면 자동으로 idle 로 전환한다(상태 파일 제거 또는 만료 처리).

**비목표**:
- Claude Code hook 페이로드 형식 변경/제안.
- 자식 프로세스가 "정말로 Claude 의 백그라운드 잡인지" 정밀 식별(예: 명령어 패턴 매칭). 1차 구현은 "살아있는 자식 존재" 단순 판정.
- `⏳` 상태에 경과 시간 표시(필요 시 별도 태스크).

**제약**:
- 감지는 `/proc/<pid>/task/<tid>/children` 또는 `/proc/<pid>/stat`의 ppid 트리 순회로 구현하며, hook 처리 시간 예산(현 `timeout budget 900ms` 정책)을 넘기지 않는다.
- 기존 우선순위 함수 `emojiForStates` 와 hook switch-case 의 유효 상태 목록을 호환 유지하며 확장한다.
- 새 상태에 대한 우선순위 / 매핑 단위 테스트와 자식 검사 함수 테스트(자식 있음/없음/PID 미존재)를 함께 추가한다.

**검증**:
- 단위 테스트: `emojiForStates` 가 `bg_waiting` 입력에 `⏳` 를 반환하고 우선순위가 `🤖` 보다 낮고 `✅` 보다 높음을 확인한다.
- 단위 테스트: 자식 프로세스 검사 함수가 fixture 디렉터리 기반으로 `true/false` 를 올바르게 반환한다.
- 수동 시나리오: tmux pane 에서 Claude 가 `Bash run_in_background` 로 장기 실행 명령을 띄운 뒤 응답을 끝낸 직후 → 윈도우 라벨이 `⏳` 로 표시된다. 백그라운드 잡 종료 후 다음 status tick 에 `⏳` 가 사라진다.

## 디자인 결정

### 디자인 결정: `bg_waiting` 감지를 "pane shell → claude → 자식 존재" 1단계 휴리스틱으로 한정
- **날짜**: 2026-05-22
- **결정**: `paneHasBackgroundJobs` 는 pane shell PID 의 직접 자식 중 `comm` 에 "claude" 가 포함된 프로세스를 찾고, 해당 claude 의 자식이 1개 이상 있을 때만 true 를 반환한다. 후손 트리 전체 탐색이나 명령어 패턴 매칭은 하지 않는다.
- **이유**: 1차 구현 목표는 "Bash run_in_background / Monitor 가 살아있는 흔한 경우" 를 잡는 것이다. 직접 자식 두 단계만 보면 /proc 접근 비용이 매우 작고(`/proc/<pid>/task/<pid>/children` 두 번), 잘못 매칭될 가능성도 낮다.
- **대안**: (1) 후손 트리 BFS 전수 조사 → 비용이 더 들고 오탐 증가. (2) `cmdline` 패턴 매칭 → 너무 fragile.

### 디자인 결정: 자동 idle 전환을 `runStatus` 렌더 경로에서 처리
- **날짜**: 2026-05-22
- **결정**: `bg_waiting` 상태 해제 책임은 1초 주기 `runStatus` (→ `aggregateWindowEmoji` → `resolvePaneStateOrClear`) 에 둔다. 자식이 사라졌으면 상태 파일을 제거해 idle 로 돌린다.
- **이유**: Claude Code 가 백그라운드 잡 종료를 별도 hook 으로 알려주지 않는다. 별도 데몬을 띄우지 않고 기존 status tick 을 재활용하면 의존성과 코드량이 최소다.
- **대안**: (1) 별도 watch 데몬 띄움 → 운영 복잡도 증가. (2) Notification hook 의존 → 잡 종료가 항상 Notification 을 부르지는 않음.

## 변경 이력

- 2026-05-22: 태스크 1(`bg_waiting` 상태 도입) 신규 등록.
- 2026-05-22: 태스크 1 완료. 디자인 결정 2건 기록.
