package trace

import (
	"io"
	"log/slog"

	"github.com/RenyEnnos/Runstead/internal/governor"
)

type PolicySink struct {
	logger *slog.Logger
}

func NewPolicySink(logger *slog.Logger) *PolicySink {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &PolicySink{logger: logger}
}

func (s *PolicySink) Emit(event governor.Event) {
	args := []any{
		"kind", event.Kind,
		"account_policy_id", event.AccountPolicyID,
		"provider", event.ProviderID,
		"model_pool", event.ModelPool,
		"model", event.Model,
		"allowance_profile", event.AllowanceProfile,
		"task_id", event.TaskID,
		"client_request_id", event.ClientRequestID,
		"attempt_sequence", event.AttemptSequence,
		"admission", event.Admission,
		"reason", event.Reason,
		"delay", event.Delay,
		"retry_at", event.RetryAt,
		"outcome", event.Outcome,
		"cooldown_until", event.CooldownUntil,
		"selected_backoff", event.SelectedBackoff,
		"circuit_from", event.CircuitFrom,
		"circuit_to", event.CircuitTo,
		"circuit_reason", event.CircuitReason,
		"telemetry_healthy", event.TelemetryHealthy,
		slog.Group("telemetry",
			"available", event.Telemetry.Available,
			"remaining", telemetryRemaining(event.Telemetry.Remaining),
			"reset_at", event.Telemetry.ResetAt,
			"cooldown_until", event.Telemetry.CooldownUntil,
			"rate_limited", event.Telemetry.RateLimited,
			"capacity_exhausted", event.Telemetry.CapacityExhausted,
			"upstream_circuit", event.Telemetry.UpstreamCircuit,
		),
		slog.Group("budgets_before",
			"rolling_3h_used", event.BudgetsBefore.Rolling3hUsed,
			"rolling_1h_used", event.BudgetsBefore.Rolling1hUsed,
			"rolling_10m_used", event.BudgetsBefore.Rolling10mUsed,
			"task_used", event.BudgetsBefore.TaskUsed,
			"retries_used", event.BudgetsBefore.RetriesUsed,
			"manual_reserve_remaining", event.BudgetsBefore.ManualReserveRemaining,
		),
		slog.Group("budgets_after",
			"rolling_3h_used", event.BudgetsAfter.Rolling3hUsed,
			"rolling_1h_used", event.BudgetsAfter.Rolling1hUsed,
			"rolling_10m_used", event.BudgetsAfter.Rolling10mUsed,
			"task_used", event.BudgetsAfter.TaskUsed,
			"retries_used", event.BudgetsAfter.RetriesUsed,
			"manual_reserve_remaining", event.BudgetsAfter.ManualReserveRemaining,
		),
	}
	if event.GatewayContractHealth != nil {
		args = append(args, slog.Group("gateway_contract_health",
			"state", event.GatewayContractHealth.State.String(),
			"reason_code", event.GatewayContractHealth.ReasonCode,
			"endpoint", event.GatewayContractHealth.Endpoint,
			"checked_at", event.GatewayContractHealth.CheckedAt,
		))
	}
	s.logger.Info("account policy event", args...)
}

func telemetryRemaining(value *int) any {
	if value == nil {
		return nil
	}
	return *value
}
