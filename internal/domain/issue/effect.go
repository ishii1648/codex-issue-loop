package issue

import "fmt"

// EffectKind identifies an idempotent external action emitted by a lifecycle
// decision. Effects are persisted independently from the Issue aggregate.
type EffectKind string

const (
	EffectNone            EffectKind = ""
	EffectMarkDone        EffectKind = "mark_done"
	EffectMarkNeedsInput  EffectKind = "mark_needs_input"
	EffectMarkFailed      EffectKind = "mark_failed"
	EffectMarkBlocked     EffectKind = "mark_blocked"
	EffectRetryConflict   EffectKind = "retry_conflict"
	EffectApplyResolution EffectKind = "apply_resolution"
)

var allEffectKinds = [...]EffectKind{
	EffectMarkDone, EffectMarkNeedsInput, EffectMarkFailed, EffectMarkBlocked,
	EffectRetryConflict, EffectApplyResolution,
}

func AllEffectKinds() []EffectKind {
	return append([]EffectKind(nil), allEffectKinds[:]...)
}

func (kind EffectKind) Validate() error {
	for _, candidate := range allEffectKinds {
		if kind == candidate {
			return nil
		}
	}
	return fmt.Errorf("unknown lifecycle effect %q", kind)
}
