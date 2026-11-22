package server

import "encoding/json"

func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }

func jsonUnmarshal(raw []byte, dst any) error { return json.Unmarshal(raw, dst) }

func jsonUnmarshalString(raw []byte, dst *string) error { return json.Unmarshal(raw, dst) }
