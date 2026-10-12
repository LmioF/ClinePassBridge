package bridge

import (
	"encoding/json"
	"strings"
)

// Keep CPA extensions, including exact JSON numbers, when a credential is
// reparsed or saved through a host callback without a resolved physical file.
func (c *Credential) UnmarshalJSON(raw []byte) error {
	type knownCredential Credential
	var known knownCredential
	if err := json.Unmarshal(raw, &known); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	for key := range fields {
		for _, knownKey := range credentialJSONFields {
			if strings.EqualFold(key, knownKey) {
				delete(fields, key)
				break
			}
		}
	}
	*c = Credential(known)
	c.extraFields = fields
	return nil
}

var credentialJSONFields = []string{
	"type", "id", "label", "api_key", "disabled", "proxy_url", "priority",
	"request_scoped_errors", "model_revision", "routing_id", "quota_block",
	"quota_identity", "quota_revision",
}

func (c Credential) MarshalJSON() ([]byte, error) {
	type knownCredential Credential
	raw, err := json.Marshal(knownCredential(c))
	if err != nil || len(c.extraFields) == 0 {
		return raw, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	for key, value := range c.extraFields {
		fields[key] = value
	}
	return json.Marshal(fields)
}
