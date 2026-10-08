package store

import "errors"

// ChatHasMessageRef reports whether slug's chat already carries an
// entry for the message at rel (a store-relative path). It is the
// check a replayed delivery makes: the tracker routes a wake, and if
// the process dies before the pending record is deleted, boot routes
// it again. The release queue dedupes by path on its own; the two
// instant paths (a CEO change straight into an agent's chat, and a
// change addressed to the CEO's inbox) append blindly, so they ask
// here first. A missing chat means nothing was delivered.
func (s *FSStore) ChatHasMessageRef(slug, rel string) (bool, error) {
	hist, err := s.ReadChatHistory(slug)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	for _, m := range hist {
		if m.MessageRef == rel {
			return true, nil
		}
	}
	return false, nil
}
