package s3api

import (
	"encoding/base64"
	"encoding/json"
)

type continuationTokenPayload struct {
	LastKey *string `json:"last_key"`
}

func EncodeContinuationToken(lastKey string) (string, error) {
	payload, err := json.Marshal(continuationTokenPayload{LastKey: &lastKey})
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(payload), nil
}

func DecodeContinuationToken(token string) (string, error) {
	payload, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return "", err
	}

	var decoded continuationTokenPayload
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return "", err
	}
	if decoded.LastKey == nil {
		return "", ErrInvalidArgumentValue
	}
	return *decoded.LastKey, nil
}
