package capy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// FileRef 是消息附件引用（上传后拿到 fileId，再随消息带上去）。
type FileRef struct {
	FileID    string `json:"fileId"`
	Name      string `json:"name"`
	MediaType string `json:"mediaType"`
	SizeBytes int64  `json:"sizeBytes"`
}

// EnrollDevice 用 Clerk 会话注册一台新设备，返回设备 id 与 capyd 令牌。
//
// 这样即使从没装过 Capy 桌面端，也能拿到设备身份去连隧道（浏览器登录 → 注册设备 → 自建设备执行）。
func (s *Session) EnrollDevice(ctx context.Context, name, hostname, osName, arch string) (orgID, deviceID, machineID, token string, err error) {
	body := map[string]any{
		"name": name, "hostname": hostname, "os": osName, "arch": arch,
		"heldOrgIds": []any{}, "deviceIds": []any{},
	}
	var out []struct {
		OrgID  string `json:"orgId"`
		Device struct {
			ID        string `json:"id"`
			MachineID string `json:"machineId"`
		} `json:"device"`
		Token string `json:"token"`
	}
	if err := s.Do(ctx, "POST", "/app/devices/enroll", "", body, &out); err != nil {
		return "", "", "", "", err
	}
	if len(out) == 0 || out[0].Token == "" {
		return "", "", "", "", fmt.Errorf("注册设备失败：上游没有返回设备凭据")
	}
	return out[0].OrgID, out[0].Device.ID, out[0].Device.MachineID, out[0].Token, nil
}

// UploadFile 走「申请预签名 URL → PUT 字节」把附件传到 Capy 的文件桶。
//
// 实测：图片作为附件带上后，模型是**直接看见图**的（不需要设备去读文件），
// 所以图片输入不需要桌面端、也不需要云机器。
func (s *Session) UploadFile(ctx context.Context, org, name, mediaType string, data []byte) (*FileRef, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("文件内容为空")
	}
	if mediaType == "" {
		mediaType = "application/octet-stream"
	}
	body := map[string]any{
		"files": []map[string]any{{
			"name":      name,
			"mediaType": mediaType,
			"sizeBytes": len(data),
		}},
	}
	var out struct {
		Targets []struct {
			FileID    string `json:"fileId"`
			URL       string `json:"url"`
			ExpiresAt string `json:"expiresAt"`
		} `json:"targets"`
	}
	if err := s.Do(ctx, "POST", fmt.Sprintf("/app/orgs/%s/files", org), org, body, &out); err != nil {
		return nil, fmt.Errorf("申请上传地址失败: %w", err)
	}
	if len(out.Targets) == 0 || out.Targets[0].URL == "" {
		return nil, fmt.Errorf("上游没有返回上传地址")
	}
	target := out.Targets[0]
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, target.URL, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("content-type", mediaType)
	req.ContentLength = int64(len(data))
	resp, err := s.Client.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("上传文件失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("上传文件失败: HTTP %d %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return &FileRef{FileID: target.FileID, Name: name, MediaType: mediaType, SizeBytes: int64(len(data))}, nil
}

// MarshalFiles 供 CreateJamRequest 使用。
func (f FileRef) MarshalJSON() ([]byte, error) {
	type alias FileRef
	return json.Marshal(alias(f))
}
