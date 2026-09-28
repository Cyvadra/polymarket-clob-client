package protocol

// Identity names the wallet one executiond trades and the strategies it
// accepts. Several executiond instances share one NATS bus, so every message
// one of them publishes says which wallet it came from.
type Identity struct {
	// WalletAddress is the address that holds the funds: the proxy wallet
	// when one is configured, otherwise the signer itself.
	WalletAddress string
	// SignerAddress is the EOA that signs the orders.
	SignerAddress     string
	AllowedStrategies []string
}

// NewIdentity builds the identity of a wallet from its signer and its maker
// (proxy wallet) address, which is empty when the signer trades its own funds.
func NewIdentity(signer, maker string, allowedStrategies []string) Identity {
	wallet := maker
	if wallet == "" {
		wallet = signer
	}
	return Identity{WalletAddress: wallet, SignerAddress: signer, AllowedStrategies: append([]string{}, allowedStrategies...)}
}

// Strategies returns a copy of the allowed strategies, never nil, so a reply
// always carries a JSON array.
func (i Identity) Strategies() []string {
	return append([]string{}, i.AllowedStrategies...)
}

type identityPublisher struct {
	publisher ExecutionEventPublisher
	identity  Identity
}

// WithIdentity wraps a publisher so that every execution message it publishes
// carries the wallet's addresses. Stamping here keeps the executor, the
// account feed, the reconciler, and the position publisher unaware of which
// wallet they run for. Other payloads pass through untouched.
func WithIdentity(publisher ExecutionEventPublisher, identity Identity) ExecutionEventPublisher {
	if publisher == nil {
		return nil
	}
	return identityPublisher{publisher: publisher, identity: identity}
}

func (p identityPublisher) PublishJSON(subject string, payload any) error {
	return p.publisher.PublishJSON(subject, p.identity.Stamp(payload))
}

// Stamp returns payload with the wallet's addresses set when it is an
// execution message, and payload itself otherwise.
func (i Identity) Stamp(payload any) any {
	switch message := payload.(type) {
	case ExecutionOpenResult:
		message.WalletAddress, message.SignerAddress = i.WalletAddress, i.SignerAddress
		return message
	case ExecutionCloseResult:
		message.WalletAddress, message.SignerAddress = i.WalletAddress, i.SignerAddress
		return message
	case ExecutionOrderEvent:
		message.WalletAddress, message.SignerAddress = i.WalletAddress, i.SignerAddress
		return message
	case PositionFeature:
		message.WalletAddress, message.SignerAddress = i.WalletAddress, i.SignerAddress
		return message
	case PositionQueryResponse:
		message.WalletAddress, message.SignerAddress = i.WalletAddress, i.SignerAddress
		message.AllowedStrategies = i.Strategies()
		positions := make([]PositionFeature, len(message.Positions))
		for index, position := range message.Positions {
			position.WalletAddress, position.SignerAddress = i.WalletAddress, i.SignerAddress
			positions[index] = position
		}
		message.Positions = positions
		return message
	case BalanceQueryResponse:
		message.WalletAddress, message.SignerAddress = i.WalletAddress, i.SignerAddress
		message.AllowedStrategies = i.Strategies()
		return message
	default:
		return payload
	}
}
