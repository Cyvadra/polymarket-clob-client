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

// Strategies returns the allowed strategies, never nil, so a reply always
// carries a JSON array. NewIdentity owns the copy; callers must not modify it.
func (i Identity) Strategies() []string {
	if i.AllowedStrategies == nil {
		return []string{}
	}
	return i.AllowedStrategies
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

// stampable is implemented by every message executiond publishes. Its
// methods have value receivers, so a pointer to a message stamps too, and a
// new message type gets its addresses by implementing it here.
type stampable interface {
	stamped(Identity) any
}

// Stamp returns payload with the wallet's addresses set when it is an
// execution message, and payload itself otherwise.
func (i Identity) Stamp(payload any) any {
	if message, ok := payload.(stampable); ok {
		return message.stamped(i)
	}
	return payload
}

func (m ExecutionOpenResult) stamped(i Identity) any {
	m.WalletAddress, m.SignerAddress = i.WalletAddress, i.SignerAddress
	return m
}

func (m ExecutionCloseResult) stamped(i Identity) any {
	m.WalletAddress, m.SignerAddress = i.WalletAddress, i.SignerAddress
	return m
}

func (m ExecutionOrderEvent) stamped(i Identity) any {
	m.WalletAddress, m.SignerAddress = i.WalletAddress, i.SignerAddress
	return m
}

func (m PositionFeature) stamped(i Identity) any {
	m.WalletAddress, m.SignerAddress = i.WalletAddress, i.SignerAddress
	return m
}

func (m PositionQueryResponse) stamped(i Identity) any {
	m.WalletAddress, m.SignerAddress = i.WalletAddress, i.SignerAddress
	m.AllowedStrategies = i.Strategies()
	positions := make([]PositionFeature, len(m.Positions))
	for index, position := range m.Positions {
		position.WalletAddress, position.SignerAddress = i.WalletAddress, i.SignerAddress
		positions[index] = position
	}
	m.Positions = positions
	return m
}

func (m BalanceQueryResponse) stamped(i Identity) any {
	m.WalletAddress, m.SignerAddress = i.WalletAddress, i.SignerAddress
	m.AllowedStrategies = i.Strategies()
	return m
}
