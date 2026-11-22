package server

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/haohaomin/capy2api/internal/capy"
	"github.com/haohaomin/capy2api/internal/store"
)

const (
	maxImages     = 8
	maxImageBytes = 20 << 20 // 单图 20MB
	maxImageTotal = 40 << 20
)

// imageInput 是解析出来、尚未上传的一张图。
type imageInput struct {
	Data      []byte
	MediaType string
	Name      string
}

// uploadImages 把图片逐张传到 Capy 文件桶，返回可随消息携带的引用。
//
// 实测：图片作为消息附件上传后，模型是**直接看见图**的（不需要设备读文件、
// 也不需要云机器），所以图片输入在 thread 模式下同样可用。
func (s *Server) uploadImages(ctx context.Context, acc store.Account, imgs []imageInput) ([]capy.FileRef, error) {
	if len(imgs) == 0 {
		return nil, nil
	}
	if len(imgs) > maxImages {
		return nil, fmt.Errorf("图片数量超过上限（最多 %d 张）", maxImages)
	}
	sess := s.sessionFor(acc)
	refs := make([]capy.FileRef, 0, len(imgs))
	var total int
	for i, img := range imgs {
		total += len(img.Data)
		if total > maxImageTotal {
			return nil, fmt.Errorf("图片总大小超过 %d MB", maxImageTotal>>20)
		}
		name := img.Name
		if name == "" {
			name = fmt.Sprintf("image-%d%s", i+1, extForMedia(img.MediaType))
		}
		ref, err := sess.UploadFile(ctx, acc.OrgID, name, img.MediaType, img.Data)
		if err != nil {
			return nil, err
		}
		refs = append(refs, *ref)
	}
	return refs, nil
}

func extForMedia(mediaType string) string {
	switch mediaType {
	case "image/png":
		return ".png"
	case "image/jpeg", "image/jpg":
		return ".jpg"
	case "image/webp":
		return ".webp"
	case "image/gif":
		return ".gif"
	default:
		return ""
	}
}

// collectImages 从 chat messages 里抽出图片，并把原消息改写成纯文本（带占位标记）。
func collectImages(messages []chatMessage) ([]chatMessage, []imageInput, error) {
	out := make([]chatMessage, 0, len(messages))
	var imgs []imageInput
	for _, m := range messages {
		if len(m.Content) == 0 || string(m.Content) == "null" {
			out = append(out, m)
			continue
		}
		var text string
		if err := jsonUnmarshalString(m.Content, &text); err == nil {
			out = append(out, m)
			continue
		}
		var parts []map[string]any
		if err := jsonUnmarshal(m.Content, &parts); err != nil {
			return nil, nil, fmt.Errorf("content 只支持字符串或内容块数组")
		}
		var b strings.Builder
		for _, part := range parts {
			typ, _ := part["type"].(string)
			switch typ {
			case "", "text", "input_text", "output_text":
				if v, ok := part["text"].(string); ok {
					b.WriteString(v)
				}
			case "image_url", "input_image":
				u := imageURLOf(part)
				if u == "" {
					return nil, nil, fmt.Errorf("图片块缺少 url")
				}
				img, err := loadImage(context.Background(), u)
				if err != nil {
					return nil, nil, err
				}
				imgs = append(imgs, img)
				b.WriteString(fmt.Sprintf("[图片%d]", len(imgs)))
			default:
				return nil, nil, fmt.Errorf("不支持的内容块类型 %q", typ)
			}
		}
		raw, _ := jsonMarshal(b.String())
		m.Content = raw
		out = append(out, m)
	}
	return out, imgs, nil
}

// imageURLOf 兼容 OpenAI（image_url:{url}）与 Responses（image_url:"..."）两种形状。
func imageURLOf(part map[string]any) string {
	if v, ok := part["image_url"].(string); ok {
		return v
	}
	if m, ok := part["image_url"].(map[string]any); ok {
		if v, ok := m["url"].(string); ok {
			return v
		}
	}
	if v, ok := part["url"].(string); ok {
		return v
	}
	return ""
}

// loadImage 解析 data: URL 或抓取远程 http(s) 图片。
func loadImage(ctx context.Context, raw string) (imageInput, error) {
	if strings.HasPrefix(raw, "data:") {
		return parseDataURL(raw)
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return imageInput{}, fmt.Errorf("图片地址只支持 data: / http(s)")
	}
	if err := guardRemoteHost(ctx, u.Hostname()); err != nil {
		return imageInput{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return imageInput{}, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return imageInput{}, fmt.Errorf("下载图片失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return imageInput{}, fmt.Errorf("下载图片失败: HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxImageBytes+1))
	if err != nil {
		return imageInput{}, err
	}
	if len(data) > maxImageBytes {
		return imageInput{}, fmt.Errorf("单张图片超过 %d MB", maxImageBytes>>20)
	}
	media := resp.Header.Get("content-type")
	if !strings.HasPrefix(media, "image/") {
		media = "image/png"
	}
	return imageInput{Data: data, MediaType: strings.TrimSpace(strings.Split(media, ";")[0])}, nil
}

func parseDataURL(raw string) (imageInput, error) {
	rest := strings.TrimPrefix(raw, "data:")
	comma := strings.Index(rest, ",")
	if comma < 0 {
		return imageInput{}, errors.New("data URL 格式错误")
	}
	meta, payload := rest[:comma], rest[comma+1:]
	media := "image/png"
	base64Encoded := false
	for _, part := range strings.Split(meta, ";") {
		if strings.HasPrefix(part, "image/") {
			media = part
		}
		if part == "base64" {
			base64Encoded = true
		}
	}
	var data []byte
	var err error
	if base64Encoded {
		data, err = base64.StdEncoding.DecodeString(payload)
		if err != nil {
			data, err = base64.RawStdEncoding.DecodeString(payload)
		}
	} else {
		var decoded string
		decoded, err = url.QueryUnescape(payload)
		data = []byte(decoded)
	}
	if err != nil {
		return imageInput{}, fmt.Errorf("解析图片 data URL 失败: %w", err)
	}
	if len(data) == 0 || len(data) > maxImageBytes {
		return imageInput{}, fmt.Errorf("图片大小为 0 或超过 %d MB", maxImageBytes>>20)
	}
	return imageInput{Data: data, MediaType: media}, nil
}

// guardRemoteHost 阻止把内网/回环地址当图片源（SSRF 兜底）。
func guardRemoteHost(ctx context.Context, host string) error {
	if host == "" {
		return errors.New("图片地址缺少主机名")
	}
	if ip := net.ParseIP(host); ip != nil {
		if blockedIP(ip) {
			return errors.New("图片地址指向内网/回环地址，已拒绝")
		}
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return fmt.Errorf("解析图片域名失败: %w", err)
	}
	for _, addr := range addrs {
		if blockedIP(addr.IP) {
			return errors.New("图片地址解析到内网/回环地址，已拒绝")
		}
	}
	return nil
}

func blockedIP(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified()
}
