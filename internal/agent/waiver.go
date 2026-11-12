package agent

import "context"

type waiverKey struct{}

// WithSkillWaiver marks ctx as a call the user's skill run preapproved, so a draft-only write may skip its prompt.
func WithSkillWaiver(ctx context.Context) context.Context {
	return context.WithValue(ctx, waiverKey{}, true)
}

// SkillWaived reports whether the call under ctx was preapproved by a skill run the user started.
func SkillWaived(ctx context.Context) bool {
	waived, _ := ctx.Value(waiverKey{}).(bool)
	return waived
}
