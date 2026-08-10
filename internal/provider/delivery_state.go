package provider

// DeliveryState is transport-level evidence about delivery of one provider
// request. Its zero value is invalid and means that delivery was unobserved.
type DeliveryState uint8

const (
	DeliveryNotSent DeliveryState = iota + 1
	DeliverySentConfirmed
	DeliverySentUnconfirmed
	DeliveryResponseStarted
	DeliveryCompleted
)

func (s DeliveryState) Valid() bool {
	return s >= DeliveryNotSent && s <= DeliveryCompleted
}

func (s DeliveryState) String() string {
	switch s {
	case DeliveryNotSent:
		return "not_sent"
	case DeliverySentConfirmed:
		return "sent_confirmed"
	case DeliverySentUnconfirmed:
		return "sent_unconfirmed"
	case DeliveryResponseStarted:
		return "response_started"
	case DeliveryCompleted:
		return "completed"
	default:
		return "unobserved"
	}
}

// ReplaySafe reports whether the observed transport evidence proves that no
// upstream model attempt was dispatched. It is a safety hint only; any new
// execution must still pass through governor admission.
func (s DeliveryState) ReplaySafe() bool { return s == DeliveryNotSent }
