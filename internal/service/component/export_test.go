package component

// SetAfterBlobCheck installs a hook that runs between Publish's blob check
// and its transaction.
func SetAfterBlobCheck(s *Service, fn func()) { s.afterBlobCheck = fn }
