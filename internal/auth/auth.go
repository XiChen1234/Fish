package auth

// Auth 固定身份鉴权表：token -> 身份 id。仅支持两个固定对端。
type Auth struct {
	tokenToID map[string]string
	ids       []string // 身份列表，约定 [0]=A [1]=B
}

func New(tokens map[string]string) *Auth {
	a := &Auth{tokenToID: make(map[string]string, len(tokens))}
	for id, tok := range tokens {
		a.tokenToID[tok] = id
		a.ids = append(a.ids, id)
	}
	return a
}

// Verify 校验 token，返回身份 id。
func (a *Auth) Verify(token string) (string, bool) {
	id, ok := a.tokenToID[token]
	return id, ok
}

// Peer 返回对端身份（一对一：两人互为对端）。未知身份返回 false。
func (a *Auth) Peer(id string) (string, bool) {
	found := false
	for _, x := range a.ids {
		if x == id {
			found = true
			break
		}
	}
	if !found {
		return "", false
	}
	for _, x := range a.ids {
		if x != id {
			return x, true
		}
	}
	return "", false
}
