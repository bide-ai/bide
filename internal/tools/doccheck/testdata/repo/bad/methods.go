package bad

// Error is an error type whose standard methods still need comments.
type Err struct{} // want `bad Err: doc comment of exported type Err should start with "Err"`

func (Err) Error() string { return "" } // want `bad Err.Error: exported method Err.Error has no doc comment`

func (Err) Unwrap() error { return nil } // want `bad Err.Unwrap: exported method Err.Unwrap has no doc comment`

func (Err) MarshalJSON() ([]byte, error) { return nil, nil } // want `bad Err.MarshalJSON: exported method Err.MarshalJSON has no doc comment`

func (*Err) UnmarshalJSON([]byte) error { return nil } // want `bad Err.UnmarshalJSON: exported method Err.UnmarshalJSON has no doc comment`

func (Err) String() string { return "" } // want `bad Err.String: exported method Err.String has no doc comment`
