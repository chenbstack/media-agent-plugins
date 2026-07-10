package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/md5"
	sha1hash "crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/chenbstack/media-agent-plugin-sdk-go/providers"
)

const (
	ossDefaultEndpoint115   = "http://oss-cn-shenzhen.aliyuncs.com"
	ossMinPartSize115       = int64(100 * 1024)
	ossPreferredPartSize115 = int64(10 * 1024 * 1024)
	ossMaxPartCount115      = int64(10000)
)

var ossSubresources115 = map[string]struct{}{
	"partNumber": {},
	"sequential": {},
	"uploadId":   {},
	"uploads":    {},
}

type uploadCallback115 struct {
	Callback    string `json:"callback"`
	CallbackVar string `json:"callback_var"`
}

type uploadInit115 struct {
	State      any               `json:"state"`
	ErrNo      int               `json:"errno"`
	Error      string            `json:"error"`
	Msg        string            `json:"msg"`
	Status     int               `json:"status"`
	StatusCode any               `json:"statuscode"`
	SignKey    string            `json:"sign_key"`
	SignCheck  string            `json:"sign_check"`
	PickCode   string            `json:"pickcode"`
	PickCode2  string            `json:"pick_code"`
	Callback   uploadCallback115 `json:"callback"`
	Bucket     string            `json:"bucket"`
	Object     string            `json:"object"`
}

type uploadResumeState115 struct {
	ParentID  int64             `json:"parent_id"`
	Target    string            `json:"target"`
	Filename  string            `json:"filename"`
	FileID    string            `json:"fileid"`
	Size      int64             `json:"size"`
	PickCode  string            `json:"pickcode"`
	Callback  uploadCallback115 `json:"callback"`
	Bucket    string            `json:"bucket"`
	Object    string            `json:"object"`
	UploadID  string            `json:"upload_id"`
	PartSize  int64             `json:"part_size"`
	UpdatedAt string            `json:"updated_at"`
}

type ossToken115 struct {
	StatusCode      string `json:"StatusCode"`
	AccessKeyID     string `json:"AccessKeyId"`
	AccessKeySecret string `json:"AccessKeySecret"`
	SecurityToken   string `json:"SecurityToken"`
	Expiration      string `json:"Expiration"`
	Endpoint        string `json:"endpoint"`
}

type ossPart115 struct {
	PartNumber     int    `xml:"PartNumber" json:"PartNumber"`
	LastModified   string `xml:"LastModified" json:"LastModified"`
	ETag           string `xml:"ETag" json:"ETag"`
	HashCrc64ecma  string `xml:"HashCrc64ecma" json:"HashCrc64ecma"`
	Size           int64  `xml:"Size" json:"Size"`
	uploadedOffset int64
}

func (c *client115) uploadFast(ctx context.Context, parentID int64, filename string, source providers.UploadSource) error {
	filename = strings.TrimSpace(filename)
	if filename == "" {
		filename = source.Name()
	}
	size := source.Size()
	if size < 0 {
		return fmt.Errorf("115 上传需要已知文件大小")
	}
	fileID, err := source.SHA1(ctx)
	if err != nil {
		return fmt.Errorf("计算 115 上传 SHA1: %w", err)
	}
	fileID = strings.ToUpper(strings.TrimSpace(fileID))
	if len(fileID) != 40 {
		return fmt.Errorf("115 上传 SHA1 无效: %s", fileID)
	}
	target := "U_1_" + strconv.FormatInt(parentID, 10)
	resumeKey := uploadResumeKey115(parentID, filename, fileID, size)

	var state uploadResumeState115
	haveState := false
	if c.kv != nil {
		ok, err := c.kv.Get(ctx, resumeKey, &state)
		if err != nil {
			return err
		}
		haveState = ok && state.FileID == fileID && state.Size == size && state.Target == target
	}

	userID, userKey, err := c.uploadIdentity(ctx)
	if err != nil {
		return err
	}
	if haveState && state.PickCode != "" {
		if resumed, err := c.uploadResume(ctx, state, fileID, size, target, userID); err == nil {
			if resumed.Object != "" && state.Object != "" && resumed.Object != state.Object {
				state.UploadID = ""
			}
			state.Object = firstNonEmpty(resumed.Object, state.Object)
			state.Bucket = firstNonEmpty(resumed.Bucket, state.Bucket)
			if resumed.Callback.Callback != "" {
				state.Callback = resumed.Callback
			}
			state.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
			_ = c.saveUploadState(ctx, resumeKey, state)
		} else {
			haveState = false
			_ = c.deleteUploadState(ctx, resumeKey)
		}
	}

	if !haveState {
		initResp, err := c.uploadInitWithVerify(ctx, parentID, filename, fileID, userID, userKey, source)
		if err != nil {
			return err
		}
		if initResp.Status == 2 {
			_ = c.deleteUploadState(ctx, resumeKey)
			return nil
		}
		if initResp.Status != 1 {
			return uploadInitError115(initResp)
		}
		state = uploadResumeState115{
			ParentID:  parentID,
			Target:    target,
			Filename:  filename,
			FileID:    fileID,
			Size:      size,
			PickCode:  initResp.pickCode(),
			Callback:  initResp.Callback,
			Bucket:    initResp.Bucket,
			Object:    initResp.Object,
			PartSize:  determinePartSize115(size),
			UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano),
		}
		if err := validateUploadState115(state); err != nil {
			return err
		}
		if err := c.saveUploadState(ctx, resumeKey, state); err != nil {
			return err
		}
	}

	token, err := c.ossToken(ctx)
	if err != nil {
		return err
	}
	endpoint := firstNonEmpty(token.Endpoint, ossDefaultEndpoint115)
	if size == 0 {
		body, err := c.ossUploadObject(ctx, token, endpoint, state.Bucket, state.Object, state.Callback)
		if err != nil {
			return err
		}
		if err := checkUploadCompleteBody115(body); err != nil {
			return err
		}
		return c.deleteUploadState(ctx, resumeKey)
	}

	if state.PartSize <= 0 {
		state.PartSize = determinePartSize115(size)
	}
	if state.UploadID == "" {
		uploadID, err := c.ossMultipartInit(ctx, token, endpoint, state.Bucket, state.Object)
		if err != nil {
			return err
		}
		state.UploadID = uploadID
		state.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
		if err := c.saveUploadState(ctx, resumeKey, state); err != nil {
			return err
		}
	}

	existing, err := c.ossListParts(ctx, token, endpoint, state.Bucket, state.Object, state.UploadID)
	if err != nil {
		return err
	}
	parts, skipSize, err := usableParts115(existing, size)
	if err != nil {
		return err
	}
	if skipSize < size {
		partNumber := len(parts) + 1
		for offset := skipSize; offset < size; {
			length := state.PartSize
			if remaining := size - offset; remaining < length {
				length = remaining
			}
			reader, err := source.OpenRange(ctx, offset, length)
			if err != nil {
				return err
			}
			part, err := c.ossUploadPart(ctx, token, endpoint, state.Bucket, state.Object, state.UploadID, partNumber, offset, length, reader)
			_ = reader.Close()
			if err != nil {
				return err
			}
			parts = append(parts, part)
			offset += length
			partNumber++
			state.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
			if err := c.saveUploadState(ctx, resumeKey, state); err != nil {
				return err
			}
		}
	}
	body, err := c.ossCompleteMultipart(ctx, token, endpoint, state.Bucket, state.Object, state.UploadID, state.Callback, parts)
	if err != nil {
		return err
	}
	if err := checkUploadCompleteBody115(body); err != nil {
		return err
	}
	return c.deleteUploadState(ctx, resumeKey)
}

func (c *client115) uploadIdentity(ctx context.Context) (string, string, error) {
	c.mu.Lock()
	if c.userID != "" && c.userKey != "" {
		userID, userKey := c.userID, c.userKey
		c.mu.Unlock()
		return userID, userKey, nil
	}
	c.mu.Unlock()

	var resp map[string]any
	if err := c.doJSON(ctx, http.MethodGet, proAPIBase+"/app/uploadinfo", nil, nil, &resp); err != nil {
		return "", "", err
	}
	if err := check115State(resp["state"], int(int64FromAny(resp["errno"])), firstNonEmpty(stringFromAny(resp["error"]), stringFromAny(resp["msg"]))); err != nil {
		return "", "", err
	}
	userID := firstNonEmpty(stringFromAny(resp["user_id"]), stringFromAny(resp["userid"]), c.userID)
	userKey := firstNonEmpty(stringFromAny(resp["userkey"]), stringFromAny(resp["user_key"]))
	if data, ok := resp["data"].(map[string]any); ok {
		userID = firstNonEmpty(userID, stringFromAny(data["user_id"]), stringFromAny(data["userid"]))
		userKey = firstNonEmpty(userKey, stringFromAny(data["userkey"]), stringFromAny(data["user_key"]))
	}
	if userID == "" || userKey == "" {
		return "", "", fmt.Errorf("115 上传信息缺少 user_id/userkey")
	}
	c.mu.Lock()
	c.userID = userID
	c.userKey = userKey
	c.mu.Unlock()
	return userID, userKey, nil
}

func (c *client115) uploadInitWithVerify(ctx context.Context, parentID int64, filename, fileID, userID, userKey string, source providers.UploadSource) (uploadInit115, error) {
	payload := map[string]string{
		"filename":  filename,
		"fileid":    fileID,
		"filesize":  strconv.FormatInt(source.Size(), 10),
		"target":    "U_1_" + strconv.FormatInt(parentID, 10),
		"topupload": "true",
		"userid":    userID,
		"userkey":   userKey,
	}
	resp, err := c.uploadInit(ctx, payload)
	if err != nil {
		return uploadInit115{}, err
	}
	if resp.Status == 7 {
		if resp.SignKey == "" || resp.SignCheck == "" {
			return uploadInit115{}, fmt.Errorf("115 二次校验响应缺少 sign_key/sign_check")
		}
		signVal, err := sourceRangeSHA1115(ctx, source, resp.SignCheck)
		if err != nil {
			return uploadInit115{}, err
		}
		payload["sign_key"] = resp.SignKey
		payload["sign_val"] = signVal
		resp, err = c.uploadInit(ctx, payload)
		if err != nil {
			return uploadInit115{}, err
		}
	}
	return resp, nil
}

func (c *client115) uploadInit(ctx context.Context, payload map[string]string) (uploadInit115, error) {
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		resp, err := c.uploadInitOnce(ctx, payload)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		if !strings.Contains(err.Error(), "HTTP 401") {
			break
		}
	}
	return uploadInit115{}, lastErr
}

func (c *client115) uploadInitOnce(ctx context.Context, payload map[string]string) (uploadInit115, error) {
	if err := c.lim.acquire(ctx); err != nil {
		return uploadInit115{}, err
	}
	kEC, body, err := makeUploadPayload115(payload, time.Now())
	if err != nil {
		return uploadInit115{}, err
	}
	query := url.Values{"k_ec": {kEC}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://uplb.115.com/4.0/initupload.php?"+query.Encode(), bytes.NewReader(body))
	if err != nil {
		return uploadInit115{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("User-Agent", uploadUserAgent115())
	req.Header.Set("Cookie", c.cookie)
	resp, err := c.http.Do(req)
	if err != nil {
		return uploadInit115{}, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return uploadInit115{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return uploadInit115{}, fmt.Errorf("115 上传初始化 HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	plain, err := uploadAESDecrypt115(data)
	if err != nil {
		return uploadInit115{}, err
	}
	var out uploadInit115
	if err := json.Unmarshal(plain, &out); err != nil {
		return uploadInit115{}, fmt.Errorf("解析 115 上传初始化响应失败: %w", err)
	}
	if out.Status == 0 && (out.Error != "" || out.Msg != "" || out.ErrNo != 0) {
		return uploadInit115{}, uploadInitError115(out)
	}
	return out, nil
}

func (c *client115) uploadResume(ctx context.Context, state uploadResumeState115, fileID string, size int64, target, userID string) (uploadInit115, error) {
	resp := uploadInit115{}
	form := url.Values{
		"pickcode": {state.PickCode},
		"target":   {target},
		"fileid":   {fileID},
		"filesize": {strconv.FormatInt(size, 10)},
		"userid":   {userID},
	}
	if err := c.doJSON(ctx, http.MethodPost, "https://uplb.115.com/3.0/resumeupload.php", nil, form, &resp); err != nil {
		return uploadInit115{}, err
	}
	if err := check115State(resp.State, resp.ErrNo, firstNonEmpty(resp.Error, resp.Msg)); err != nil {
		return uploadInit115{}, err
	}
	return resp, nil
}

func (c *client115) ossToken(ctx context.Context) (ossToken115, error) {
	c.mu.Lock()
	if c.token.valid() {
		token := c.token
		c.mu.Unlock()
		return token, nil
	}
	c.mu.Unlock()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://uplb.115.com/3.0/gettoken.php", nil)
	if err != nil {
		return ossToken115{}, err
	}
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("User-Agent", uploadUserAgent115())
	resp, err := c.http.Do(req)
	if err != nil {
		return ossToken115{}, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return ossToken115{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return ossToken115{}, fmt.Errorf("115 OSS token HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	var token ossToken115
	if err := json.Unmarshal(data, &token); err != nil {
		return ossToken115{}, fmt.Errorf("解析 115 OSS token 失败: %w", err)
	}
	if token.StatusCode != "" && token.StatusCode != "200" {
		return ossToken115{}, fmt.Errorf("115 OSS token 失败: %s", token.StatusCode)
	}
	if token.AccessKeyID == "" || token.AccessKeySecret == "" || token.SecurityToken == "" {
		return ossToken115{}, fmt.Errorf("115 OSS token 响应不完整")
	}
	c.mu.Lock()
	c.token = token
	c.mu.Unlock()
	return token, nil
}

func (c *client115) ossUploadObject(ctx context.Context, token ossToken115, endpoint, bucket, object string, callback uploadCallback115) ([]byte, error) {
	headers := http.Header{}
	headers.Set("x-oss-callback", base64.StdEncoding.EncodeToString([]byte(callback.Callback)))
	headers.Set("x-oss-callback-var", base64.StdEncoding.EncodeToString([]byte(callback.CallbackVar)))
	return c.ossDo(ctx, token, http.MethodPut, endpoint, bucket, object, nil, headers, http.NoBody, 0)
}

func (c *client115) ossMultipartInit(ctx context.Context, token ossToken115, endpoint, bucket, object string) (string, error) {
	query := url.Values{"sequential": {"1"}, "uploads": {"1"}}
	data, err := c.ossDo(ctx, token, http.MethodPost, endpoint, bucket, object, query, nil, nil, -1)
	if err != nil {
		return "", err
	}
	var parsed struct {
		UploadID string `xml:"UploadId"`
	}
	if err := xml.Unmarshal(data, &parsed); err != nil {
		return "", fmt.Errorf("解析 OSS multipart 初始化响应失败: %w", err)
	}
	if parsed.UploadID == "" {
		return "", fmt.Errorf("OSS multipart 初始化未返回 upload_id")
	}
	return parsed.UploadID, nil
}

func (c *client115) ossListParts(ctx context.Context, token ossToken115, endpoint, bucket, object, uploadID string) ([]ossPart115, error) {
	var out []ossPart115
	marker := 0
	for {
		query := url.Values{
			"uploadId":           {uploadID},
			"part-number-marker": {strconv.Itoa(marker)},
		}
		data, err := c.ossDo(ctx, token, http.MethodGet, endpoint, bucket, object, query, nil, nil, -1)
		if err != nil {
			return nil, err
		}
		var parsed struct {
			IsTruncated         bool         `xml:"IsTruncated"`
			NextPartNumberMark  int          `xml:"NextPartNumberMarker"`
			NextPartNumberMark2 int          `xml:"NextMarker"`
			Parts               []ossPart115 `xml:"Part"`
		}
		if err := xml.Unmarshal(data, &parsed); err != nil {
			return nil, fmt.Errorf("解析 OSS 分片列表失败: %w", err)
		}
		out = append(out, parsed.Parts...)
		if !parsed.IsTruncated {
			break
		}
		marker = parsed.NextPartNumberMark
		if marker == 0 {
			marker = parsed.NextPartNumberMark2
		}
		if marker == 0 {
			break
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PartNumber < out[j].PartNumber })
	return out, nil
}

func (c *client115) ossUploadPart(ctx context.Context, token ossToken115, endpoint, bucket, object, uploadID string, partNumber int, offset, size int64, reader io.Reader) (ossPart115, error) {
	query := url.Values{
		"partNumber": {strconv.Itoa(partNumber)},
		"uploadId":   {uploadID},
	}
	hash := md5.New()
	body := io.TeeReader(reader, hash)
	headers, data, err := c.ossDoWithHeaders(ctx, token, http.MethodPut, endpoint, bucket, object, query, nil, body, size)
	if err != nil {
		return ossPart115{}, err
	}
	_ = data
	md5Local := strings.ToUpper(hex.EncodeToString(hash.Sum(nil)))
	etag := headers.Get("ETag")
	if strings.Trim(etag, "\"") != md5Local {
		return ossPart115{}, fmt.Errorf("OSS 分片 %d MD5 不一致: %s != %s", partNumber, md5Local, etag)
	}
	return ossPart115{
		PartNumber:     partNumber,
		LastModified:   headers.Get("Date"),
		ETag:           etag,
		HashCrc64ecma:  headers.Get("x-oss-hash-crc64ecma"),
		Size:           size,
		uploadedOffset: offset,
	}, nil
}

func (c *client115) ossCompleteMultipart(ctx context.Context, token ossToken115, endpoint, bucket, object, uploadID string, callback uploadCallback115, parts []ossPart115) ([]byte, error) {
	var body strings.Builder
	body.WriteString("<CompleteMultipartUpload>")
	for _, part := range parts {
		body.WriteString("<Part><PartNumber>")
		body.WriteString(strconv.Itoa(part.PartNumber))
		body.WriteString("</PartNumber><ETag>")
		xml.EscapeText(&body, []byte(part.ETag))
		body.WriteString("</ETag></Part>")
	}
	body.WriteString("</CompleteMultipartUpload>")

	headers := http.Header{}
	headers.Set("Content-Type", "text/xml")
	headers.Set("x-oss-callback", base64.StdEncoding.EncodeToString([]byte(callback.Callback)))
	headers.Set("x-oss-callback-var", base64.StdEncoding.EncodeToString([]byte(callback.CallbackVar)))
	query := url.Values{"uploadId": {uploadID}}
	return c.ossDo(ctx, token, http.MethodPost, endpoint, bucket, object, query, headers, strings.NewReader(body.String()), int64(body.Len()))
}

func (c *client115) ossDo(ctx context.Context, token ossToken115, method, endpoint, bucket, object string, query url.Values, headers http.Header, body io.Reader, contentLength int64) ([]byte, error) {
	_, data, err := c.ossDoWithHeaders(ctx, token, method, endpoint, bucket, object, query, headers, body, contentLength)
	return data, err
}

func (c *client115) ossDoWithHeaders(ctx context.Context, token ossToken115, method, endpoint, bucket, object string, query url.Values, headers http.Header, body io.Reader, contentLength int64) (http.Header, []byte, error) {
	reqURL, err := ossURL115(endpoint, bucket, object, query)
	if err != nil {
		return nil, nil, err
	}
	if headers == nil {
		headers = http.Header{}
	}
	signed := ossSign115(token, method, reqURL, headers)
	req, err := http.NewRequestWithContext(ctx, method, reqURL, body)
	if err != nil {
		return nil, nil, err
	}
	for key, values := range signed {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}
	req.Header.Set("User-Agent", uploadUserAgent115())
	if contentLength >= 0 {
		req.ContentLength = contentLength
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, nil, fmt.Errorf("115 OSS %s HTTP %d: %s", method, resp.StatusCode, strings.TrimSpace(string(data)))
	}
	return resp.Header, data, nil
}

func ossURL115(endpoint, bucket, object string, query url.Values) (string, error) {
	endpoint = firstNonEmpty(strings.TrimSpace(endpoint), ossDefaultEndpoint115)
	if !strings.Contains(endpoint, "://") {
		endpoint = "http://" + endpoint
	}
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return "", err
	}
	host := parsed.Host
	if host == "" {
		return "", fmt.Errorf("OSS endpoint 无效: %s", endpoint)
	}
	if !strings.Contains(host, ".") {
		host += ".aliyuncs.com"
	}
	if bucket != "" && !strings.HasPrefix(host, bucket+".") {
		host = bucket + "." + host
	}
	out := url.URL{Scheme: firstNonEmpty(parsed.Scheme, "http"), Host: host}
	out.Path = "/" + strings.TrimLeft(object, "/")
	if len(query) > 0 {
		out.RawQuery = query.Encode()
	}
	return out.String(), nil
}

func ossSign115(token ossToken115, method, reqURL string, headers http.Header) http.Header {
	signed := http.Header{}
	for key, values := range headers {
		for _, value := range values {
			signed.Add(key, value)
		}
	}
	signed.Set("x-oss-security-token", token.SecurityToken)
	if signed.Get("Date") == "" && signed.Get("x-oss-date") == "" {
		signed.Set("Date", time.Now().UTC().Format(http.TimeFormat))
	}
	date := firstNonEmpty(signed.Get("x-oss-date"), signed.Get("Date"))
	contentMD5 := signed.Get("Content-MD5")
	contentType := signed.Get("Content-Type")

	parsed, _ := url.Parse(reqURL)
	bucket := strings.SplitN(parsed.Hostname(), ".", 2)[0]
	canonicalHeaders := canonicalOSSHeaders115(signed)
	canonicalResource := "/" + bucket + parsed.EscapedPath() + canonicalOSSQuery115(parsed.RawQuery)
	stringToSign := strings.Join([]string{
		strings.ToUpper(method),
		contentMD5,
		contentType,
		date,
		canonicalHeaders + canonicalResource,
	}, "\n")
	mac := hmac.New(sha1hash.New, []byte(token.AccessKeySecret))
	mac.Write([]byte(stringToSign))
	signature := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	signed.Set("Authorization", "OSS "+token.AccessKeyID+":"+signature)
	return signed
}

func canonicalOSSHeaders115(headers http.Header) string {
	type pair struct {
		key   string
		value string
	}
	var pairs []pair
	for key, values := range headers {
		lower := strings.ToLower(key)
		if !strings.HasPrefix(lower, "x-oss-") {
			continue
		}
		pairs = append(pairs, pair{key: lower, value: strings.Join(values, ",")})
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].key == pairs[j].key {
			return pairs[i].value < pairs[j].value
		}
		return pairs[i].key < pairs[j].key
	})
	var b strings.Builder
	for _, p := range pairs {
		b.WriteString(p.key)
		b.WriteByte(':')
		b.WriteString(p.value)
		b.WriteByte('\n')
	}
	return b.String()
}

func canonicalOSSQuery115(rawQuery string) string {
	if rawQuery == "" {
		return ""
	}
	values, _ := url.ParseQuery(rawQuery)
	filtered := url.Values{}
	for key, vals := range values {
		if _, ok := ossSubresources115[key]; !ok {
			continue
		}
		for _, value := range vals {
			filtered.Add(key, value)
		}
	}
	if len(filtered) == 0 {
		return ""
	}
	return "?" + filtered.Encode()
}

func sourceRangeSHA1115(ctx context.Context, source providers.UploadSource, signCheck string) (string, error) {
	start, end, err := parseInclusiveRange115(signCheck)
	if err != nil {
		return "", err
	}
	reader, err := source.OpenRange(ctx, start, end-start+1)
	if err != nil {
		return "", err
	}
	defer reader.Close()
	h := sha1hash.New()
	if _, err := io.Copy(h, reader); err != nil {
		return "", err
	}
	return strings.ToUpper(hex.EncodeToString(h.Sum(nil))), nil
}

func parseInclusiveRange115(value string) (int64, int64, error) {
	left, right, ok := strings.Cut(strings.TrimSpace(value), "-")
	if !ok {
		return 0, 0, fmt.Errorf("115 sign_check 范围无效: %s", value)
	}
	start, err := strconv.ParseInt(left, 10, 64)
	if err != nil {
		return 0, 0, err
	}
	end, err := strconv.ParseInt(right, 10, 64)
	if err != nil {
		return 0, 0, err
	}
	if start < 0 || end < start {
		return 0, 0, fmt.Errorf("115 sign_check 范围无效: %s", value)
	}
	return start, end, nil
}

func usableParts115(parts []ossPart115, size int64) ([]ossPart115, int64, error) {
	sort.Slice(parts, func(i, j int) bool { return parts[i].PartNumber < parts[j].PartNumber })
	out := make([]ossPart115, 0, len(parts))
	var skip int64
	next := 1
	for _, part := range parts {
		if part.PartNumber != next {
			break
		}
		if part.Size < 0 {
			return nil, 0, fmt.Errorf("OSS 分片大小无效")
		}
		isFinal := skip+part.Size == size
		if part.Size < ossMinPartSize115 && !isFinal {
			break
		}
		part.uploadedOffset = skip
		out = append(out, part)
		skip += part.Size
		next++
		if skip == size {
			break
		}
		if skip > size {
			return nil, 0, fmt.Errorf("OSS 已上传分片超过文件大小")
		}
	}
	return out, skip, nil
}

func determinePartSize115(size int64) int64 {
	partSize := ossPreferredPartSize115
	if size <= ossMinPartSize115 {
		return ossMinPartSize115
	}
	needed := int64(math.Ceil(float64(size) / float64(ossMaxPartCount115)))
	for partSize < needed {
		partSize <<= 1
	}
	return partSize
}

func uploadResumeKey115(parentID int64, filename, fileID string, size int64) string {
	sum := sha1hash.Sum([]byte(strconv.FormatInt(parentID, 10) + "\x00" + filename + "\x00" + fileID + "\x00" + strconv.FormatInt(size, 10)))
	return "drive115/upload/" + hex.EncodeToString(sum[:])
}

func (c *client115) saveUploadState(ctx context.Context, key string, state uploadResumeState115) error {
	if c.kv == nil {
		return nil
	}
	return c.kv.Set(ctx, key, state, 0)
}

func (c *client115) deleteUploadState(ctx context.Context, key string) error {
	if c.kv == nil {
		return nil
	}
	return c.kv.Delete(ctx, key)
}

func validateUploadState115(state uploadResumeState115) error {
	if state.PickCode == "" || state.Bucket == "" || state.Object == "" {
		return fmt.Errorf("115 上传初始化响应不完整")
	}
	if state.Callback.Callback == "" || state.Callback.CallbackVar == "" {
		return fmt.Errorf("115 上传 callback 响应不完整")
	}
	return nil
}

func (r uploadInit115) pickCode() string {
	return firstNonEmpty(r.PickCode, r.PickCode2)
}

func (t ossToken115) valid() bool {
	if t.AccessKeyID == "" || t.AccessKeySecret == "" || t.SecurityToken == "" {
		return false
	}
	if t.Expiration == "" {
		return true
	}
	expires, err := time.Parse(time.RFC3339, t.Expiration)
	if err != nil {
		return true
	}
	return time.Now().UTC().Add(10 * time.Minute).Before(expires)
}

func uploadUserAgent115() string {
	return "Mozilla/5.0 115disk/" + uploadAppVersion115 + " 115Browser/" + uploadAppVersion115 + " 115wangpan_android/" + uploadAppVersion115
}

func uploadInitError115(resp uploadInit115) error {
	msg := firstNonEmpty(resp.Error, resp.Msg)
	if msg == "" {
		msg = "115 上传初始化失败"
	}
	if resp.Status != 0 {
		msg += " status=" + strconv.Itoa(resp.Status)
	}
	if code := stringFromAny(resp.StatusCode); code != "" {
		msg += " statuscode=" + code
	}
	return fmt.Errorf("%s", msg)
}

func checkUploadCompleteBody115(body []byte) error {
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		return nil
	}
	var simple simple115Response
	if err := json.Unmarshal(body, &simple); err == nil && (simple.State != nil || simple.ErrNo != 0 || simple.Error != "" || simple.Msg != "") {
		return check115State(simple.State, simple.ErrNo, firstNonEmpty(simple.Error, simple.Msg))
	}
	return nil
}
