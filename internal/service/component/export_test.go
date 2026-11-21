package component

import "time"

// SetAfterBlobCheck installs a hook that runs between Publish's blob check
// and its transaction.
func SetAfterBlobCheck(s *Service, fn func()) { s.afterBlobCheck = fn }

// PinValidity is how long after the blob check a publish may still revive
// an unreferenced blob.
const PinValidity = pinValidity

// SetPinValidity overrides PinValidity for s.
func SetPinValidity(s *Service, d time.Duration) { s.pinValidity = d }
