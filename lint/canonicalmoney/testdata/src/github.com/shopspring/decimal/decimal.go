package decimal

type Decimal struct{}

func NewFromString(s string) (Decimal, error) { return Decimal{}, nil }
func RequireFromString(s string) Decimal      { return Decimal{} }
