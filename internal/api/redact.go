package api

import "go-codearts/internal/store"

// redactAccount returns a detached account view safe for API responses.
// It never exposes passwords, secret keys, session tokens, or DPoP private keys.
func redactAccount(src *store.Account) *store.Account {
	if src == nil {
		return nil
	}
	cp := *src
	cp.Password = ""
	cp.RefreshToken = ""
	if src.Metadata != nil {
		cp.Metadata = make(map[string]string, len(src.Metadata))
		for k, v := range src.Metadata {
			if k == "user_token" || k == "access_token" || k == "refresh_token" {
				continue
			}
			cp.Metadata[k] = v
		}
	}
	if src.Credentials != nil {
		cred := *src.Credentials
		cred.SecretAccessKey = ""
		cred.SecurityToken = ""
		if src.Credentials.DpopKeyPair != nil {
			pair := *src.Credentials.DpopKeyPair
			pair.PrivateKeyJwk = ""
			cred.DpopKeyPair = &pair
		}
		cp.Credentials = &cred
	}
	return &cp
}

func redactAccounts(src []*store.Account) []*store.Account {
	out := make([]*store.Account, 0, len(src))
	for _, a := range src {
		out = append(out, redactAccount(a))
	}
	return out
}
