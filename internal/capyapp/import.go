// Package capyapp 从 macOS 上安装的 Capy 桌面端直接读取登录凭据。
//
// Capy 用 Electron safeStorage 加密 session-credential.json：
// 密钥在钥匙串（service="Capy Safe Storage"），派生方式与 Chromium 相同：
// PBKDF2-HMAC-SHA1(password, "saltysalt", 1003, 16)，AES-128-CBC，IV 为 16 个空格，
// 明文前 3 字节是 "v10"/"v11" 版本前缀。
package capyapp

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// OrgDevice 是组织与设备的绑定。
type OrgDevice struct {
	OrgID    string `json:"org_id"`
	DeviceID string `json:"device_id"`
	UserID   string `json:"user_id,omitempty"`
	Token    string `json:"-"` // 设备凭据（capyd_...），用于直连 device tunnel
}

// Detected 是从本机 Capy 应用里读到的账号信息。
type Detected struct {
	Path      string      `json:"path"`
	UserID    string      `json:"user_id"`
	SessionID string      `json:"session_id"`
	Cookie    string      `json:"-"`
	Orgs      []OrgDevice `json:"orgs"`
	Keychain  string      `json:"keychain,omitempty"`
}

// Dir 返回 Capy 桌面端数据目录。
func Dir() string {
	if v := strings.TrimSpace(os.Getenv("CAPY2API_CAPY_HOME")); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, "Library", "Application Support", "Capy")
}

// Available 判断当前机器上是否存在可导入的 Capy 登录态。
func Available() bool {
	dir := Dir()
	if dir == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(dir, "session-credential.json"))
	return err == nil
}

// AppRunning 判断 Capy 桌面端是否在运行。
func AppRunning() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "pgrep", "-x", "Capy").Output()
	return err == nil && len(bytes.TrimSpace(out)) > 0
}

type sessionFile struct {
	Version    int    `json:"version"`
	UserID     string `json:"userId"`
	SessionID  string `json:"sessionId"`
	Ciphertext string `json:"ciphertext"`
}

type deviceFile struct {
	Version int `json:"version"`
	Orgs    map[string]struct {
		UserID     string `json:"userId"`
		DeviceID   string `json:"deviceId"`
		Ciphertext string `json:"ciphertext"`
	} `json:"orgs"`
}

type idsFile struct {
	Version int               `json:"version"`
	Orgs    map[string]string `json:"orgs"`
}

// Detect 读取并解密本机 Capy 登录态。
//
// 需要系统钥匙串里有 "Capy Safe Storage" 条目；首次读取时 macOS 会弹窗确认，
// 点"允许"即可（也可以预先在"钥匙串访问"里给 security 命令授权）。
func Detect(ctx context.Context) (*Detected, error) {
	dir := Dir()
	if dir == "" {
		return nil, errors.New("无法定位用户目录")
	}
	raw, err := os.ReadFile(filepath.Join(dir, "session-credential.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, errors.New("没有找到 Capy 登录态，请先打开 Capy 桌面端并完成登录")
		}
		return nil, err
	}
	var sf sessionFile
	if err := json.Unmarshal(raw, &sf); err != nil {
		return nil, fmt.Errorf("解析 session-credential.json 失败: %w", err)
	}
	secret, err := keychainSecret(ctx)
	if err != nil {
		return nil, err
	}
	cookie, err := decryptString(sf.Ciphertext, secret)
	if err != nil {
		return nil, fmt.Errorf("解密 Capy 会话失败: %w", err)
	}
	out := &Detected{Path: dir, UserID: sf.UserID, SessionID: sf.SessionID, Cookie: cookie}

	if devRaw, err := os.ReadFile(filepath.Join(dir, "device-credentials.json")); err == nil {
		var df deviceFile
		if json.Unmarshal(devRaw, &df) == nil {
			for org, item := range df.Orgs {
				token := ""
				if item.Ciphertext != "" {
					if dec, derr := decryptString(item.Ciphertext, secret); derr == nil {
						token = strings.TrimSpace(dec)
					}
				}
				out.Orgs = append(out.Orgs, OrgDevice{OrgID: org, DeviceID: item.DeviceID, UserID: item.UserID, Token: token})
			}
		}
	}
	if idsRaw, err := os.ReadFile(filepath.Join(dir, "device-ids.json")); err == nil {
		var ids idsFile
		if json.Unmarshal(idsRaw, &ids) == nil {
			for org, device := range ids.Orgs {
				found := false
				for _, o := range out.Orgs {
					if o.OrgID == org {
						found = true
						break
					}
				}
				if !found {
					out.Orgs = append(out.Orgs, OrgDevice{OrgID: org, DeviceID: device})
				}
			}
		}
	}
	if len(out.Orgs) == 0 {
		out.Orgs = append(out.Orgs, OrgDevice{})
	}
	return out, nil
}

func keychainSecret(ctx context.Context) (string, error) {
	service := strings.TrimSpace(os.Getenv("CAPY2API_KEYCHAIN_SERVICE"))
	if service == "" {
		service = "Capy Safe Storage"
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, "security", "find-generic-password", "-s", service, "-w").Output()
	if err != nil {
		return "", fmt.Errorf("读取钥匙串 %q 失败（首次需要允许 security 访问）: %w", service, err)
	}
	secret := strings.TrimSpace(string(out))
	if secret == "" {
		return "", errors.New("钥匙串返回空密码")
	}
	return secret, nil
}

func decryptString(b64, secret string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
	if err != nil {
		return "", err
	}
	if len(raw) < 3 {
		return "", errors.New("密文过短")
	}
	prefix := string(raw[:3])
	if prefix != "v10" && prefix != "v11" {
		// 未加密（部分平台明文存储）。
		return string(raw), nil
	}
	key := pbkdf2SHA1([]byte(secret), []byte("saltysalt"), 1003, 16)
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	data := raw[3:]
	if len(data)%aes.BlockSize != 0 {
		return "", errors.New("密文长度不是块大小的整数倍")
	}
	iv := bytes.Repeat([]byte{' '}, aes.BlockSize)
	out := make([]byte, len(data))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(out, data)
	// 去掉 PKCS#5/PKCS#7 填充（Electron 在 macOS 上不填充，这里按需处理）。
	if n := len(out); n > 0 {
		pad := int(out[n-1])
		if pad > 0 && pad <= aes.BlockSize && pad <= n {
			valid := true
			for _, b := range out[n-pad:] {
				if int(b) != pad {
					valid = false
					break
				}
			}
			if valid {
				out = out[:n-pad]
			}
		}
	}
	return string(out), nil
}

func pbkdf2SHA1(password, salt []byte, iter, keyLen int) []byte {
	prf := hmac.New(sha1.New, password)
	hashLen := prf.Size()
	blocks := (keyLen + hashLen - 1) / hashLen
	out := make([]byte, 0, blocks*hashLen)
	buf := make([]byte, 4)
	for block := 1; block <= blocks; block++ {
		prf.Reset()
		prf.Write(salt)
		buf[0] = byte(block >> 24)
		buf[1] = byte(block >> 16)
		buf[2] = byte(block >> 8)
		buf[3] = byte(block)
		prf.Write(buf)
		u := prf.Sum(nil)
		t := make([]byte, len(u))
		copy(t, u)
		for i := 1; i < iter; i++ {
			prf.Reset()
			prf.Write(u)
			u = prf.Sum(u[:0])
			for j := range t {
				t[j] ^= u[j]
			}
		}
		out = append(out, t...)
	}
	return out[:keyLen]
}
