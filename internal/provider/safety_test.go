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
