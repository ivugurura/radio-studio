package netutil

import (
	"encoding/json"
	"net/http"
)

type ServerRes struct {
	Message  string `json:"message,omitempty"`
	ErrorMsg string `json:"error_msg,omitempty"`
	Data     any    `json:"data,omitempty"`
	Success  bool   `json:"success"`
}

func NewServerRes() *ServerRes {
	return &ServerRes{
		Success: false,
	}
}

func ServerResponse(w http.ResponseWriter, code int, message string, data any) {
	sr := NewServerRes()
	if code < 400 {
		sr.Success = true
		sr.Message = message
		sr.Data = data
	} else {
		sr.ErrorMsg = message
	}
	w.WriteHeader(code)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(sr)
}
