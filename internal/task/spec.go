package task

import "time"

// Spec is a validated, resolved task definition. Treat its slices and maps as
// immutable after construction; configuration accessors return deep copies.
type Spec struct {
	Name        string
	Description string
	Argv        []string
	Dir         string
	Env         map[string]string
	Timeout     time.Duration
}
