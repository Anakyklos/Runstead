package provider

import "testing"

func TestRouteSafetyRequiresExplicitSingleAttemptAndDisabledAmplification(t *testing.T) {
	if err := SafeRouteSafety().Validate(); err != nil {
		t.Fatalf("SafeRouteSafety().Validate() error = %v", err)
	}
	cases := []RouteSafety{
		{},
		{SingleAttempt: SingleAttemptGuaranteed},
		{SingleAttempt: SingleAttemptGuaranteed, InternalRetries: AmplificationEnabled, CooldownReplay: AmplificationDisabled, AccountPooling: AmplificationDisabled, AutomaticFallback: AmplificationDisabled},
		{SingleAttempt: SingleAttemptGuaranteed, InternalRetries: AmplificationDisabled, CooldownReplay: AmplificationEnabled, AccountPooling: AmplificationDisabled, AutomaticFallback: AmplificationDisabled},
		{SingleAttempt: SingleAttemptGuaranteed, InternalRetries: AmplificationDisabled, CooldownReplay: AmplificationDisabled, AccountPooling: AmplificationEnabled, AutomaticFallback: AmplificationDisabled},
		{SingleAttempt: SingleAttemptGuaranteed, InternalRetries: AmplificationDisabled, CooldownReplay: AmplificationDisabled, AccountPooling: AmplificationDisabled, AutomaticFallback: AmplificationEnabled},
	}
	for _, safety := range cases {
		if err := safety.Validate(); err == nil {
			t.Fatalf("RouteSafety %#v was accepted", safety)
		}
	}
}

func TestReceiptRouteSafetyDisablesMultiTargetAmplificationInM1(t *testing.T) {
	safety := ReceiptRouteSafety()
	if err := safety.Validate(); err != nil {
		t.Fatalf("ReceiptRouteSafety().Validate() error = %v", err)
	}
	if safety.AccountPooling != AmplificationDisabled || safety.AutomaticFallback != AmplificationDisabled || safety.ComboRouting != AmplificationDisabled {
		t.Fatalf("M1 receipt safety = %#v, want pooling/fallback/combo disabled", safety)
	}
	for name, mutate := range map[string]func(*RouteSafety){
		"account pooling":    func(value *RouteSafety) { value.AccountPooling = AmplificationEnabled },
		"automatic fallback": func(value *RouteSafety) { value.AutomaticFallback = AmplificationEnabled },
		"combo routing":      func(value *RouteSafety) { value.ComboRouting = AmplificationEnabled },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := safety
			mutate(&candidate)
			if err := candidate.Validate(); err == nil {
				t.Fatalf("M1 route with %s enabled was accepted", name)
			}
		})
	}
}
