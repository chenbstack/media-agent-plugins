package main

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash/crc32"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

const uploadAppVersion115 = "36.2.28"

var (
	uploadCRCSalt115 = []byte("^j>WD3Kr?J2gLFjD4W2y@")
	uploadMD5Salt115 = []byte("Qclm8MGWUv59TnrR0XPg")
	uploadAESPub115  = []byte{
		0x1d, 0x03, 0x0e, 0x80, 0xa1, 0x78, 0xdc, 0xee, 0xce, 0xcd, 0xa3, 0x77, 0xde, 0x12, 0x8d,
		0x8e, 0xd9, 0xdd, 0xcf, 0x55, 0xae, 0x61, 0xed, 0x46, 0xea, 0x12, 0x1a, 0x1c, 0xfc, 0x81,
	}
	uploadAESKey115 = []byte{0xfb, 0x1a, 0x19, 0xd6, 0x52, 0xf5, 0xaa, 0xf7, 0xbc, 0x65, 0x1d, 0x0f, 0x69, 0xbf, 0x42, 0x2f}
	uploadAESIV115  = []byte{0x69, 0xbf, 0x42, 0x2f, 0x49, 0x96, 0x05, 0x50, 0xa0, 0xad, 0x44, 0xec, 0x34, 0x46, 0xcb, 0x4c}
)

func makeUploadPayload115(payload map[string]string, now time.Time) (string, []byte, error) {
	p := map[string]string{}
	for k, v := range payload {
		p[k] = v
	}
	p["appversion"] = firstNonEmpty(p["appversion"], uploadAppVersion115)
	t := now.Unix()
	p["t"] = strconv.FormatInt(t, 10)

	userID := p["userid"]
	if userID == "" {
		return "", nil, fmt.Errorf("115 上传缺少用户 ID")
	}
	userIDInt, err := strconv.ParseInt(userID, 10, 64)
	if err != nil {
		return "", nil, fmt.Errorf("115 用户 ID 无效: %w", err)
	}
	fileID := strings.ToUpper(p["fileid"])
	p["fileid"] = fileID

	inner := sha1.Sum([]byte(userID + fileID + p["target"] + "0"))
	sig := sha1.New()
	sig.Write([]byte(p["userkey"]))
	sig.Write([]byte(hex.EncodeToString(inner[:])))
	sig.Write([]byte("000000"))
	p["sig"] = strings.ToUpper(hex.EncodeToString(sig.Sum(nil)))

	token := md5.New()
	token.Write(uploadMD5Salt115)
	token.Write([]byte(fileID + p["filesize"] + p["sign_key"] + p["sign_val"] + userID + p["t"]))
	userMD5 := md5.Sum([]byte(strconv.FormatInt(userIDInt, 10)))
	token.Write([]byte(hex.EncodeToString(userMD5[:])))
	token.Write([]byte(p["appversion"]))
	p["token"] = hex.EncodeToString(token.Sum(nil))

	values := make(url.Values, len(p))
	keys := make([]string, 0, len(p))
	for key, value := range p {
		if value == "" {
			continue
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		values.Set(key, p[key])
	}
	encrypted, err := uploadAESEncrypt115([]byte(values.Encode()))
	if err != nil {
		return "", nil, err
	}
	return uploadEncodeToken115(t), encrypted, nil
}

func uploadEncodeToken115(timestamp int64) string {
	token := make([]byte, 0, 48)
	token = append(token, uploadAESPub115[:15]...)
	token = append(token, 0x00, 0x73, 0x00, 0x00, 0x00)
	var ts [4]byte
	binary.LittleEndian.PutUint32(ts[:], uint32(timestamp))
	token = append(token, ts[:]...)
	token = append(token, uploadAESPub115[15:]...)
	token = append(token, 0x00, 0x01, 0x00, 0x00, 0x00)
	crcInput := make([]byte, 0, len(uploadCRCSalt115)+len(token))
	crcInput = append(crcInput, uploadCRCSalt115...)
	crcInput = append(crcInput, token...)
	crc := crc32.ChecksumIEEE(crcInput)
	var sum [4]byte
	binary.LittleEndian.PutUint32(sum[:], crc)
	token = append(token, sum[:]...)
	return base64.StdEncoding.EncodeToString(token)
}

func uploadAESEncrypt115(data []byte) ([]byte, error) {
	block, err := aes.NewCipher(uploadAESKey115)
	if err != nil {
		return nil, err
	}
	data = uploadPad115(data)
	if len(data) == 0 {
		return nil, nil
	}
	out := make([]byte, len(data))
	cipher.NewCBCEncrypter(block, uploadAESIV115).CryptBlocks(out, data)
	return out, nil
}

func uploadAESDecrypt115(data []byte) ([]byte, error) {
	block, err := aes.NewCipher(uploadAESKey115)
	if err != nil {
		return nil, err
	}
	if n := len(data) &^ 15; n != len(data) {
		data = data[:n]
	}
	if len(data) == 0 {
		return nil, nil
	}
	out := make([]byte, len(data))
	cipher.NewCBCDecrypter(block, uploadAESIV115).CryptBlocks(out, data)
	plain := uploadUnpad115(out)
	decompressed, err := uploadLZ4Decompress115(plain)
	if err != nil && bytes.HasPrefix(bytes.TrimSpace(plain), []byte("{")) {
		return plain, nil
	}
	return decompressed, err
}

func uploadPad115(data []byte) []byte {
	pad := -len(data) & 15
	if pad == 0 {
		return append([]byte{}, data...)
	}
	out := append([]byte{}, data...)
	for range pad {
		out = append(out, byte(pad))
	}
	return out
}

func uploadUnpad115(data []byte) []byte {
	if len(data) == 0 {
		return data
	}
	pad := int(data[len(data)-1])
	if pad <= 0 || pad >= 16 || pad > len(data) {
		return data
	}
	for _, b := range data[len(data)-pad:] {
		if int(b) != pad {
			return data
		}
	}
	return data[:len(data)-pad]
}

func uploadLZ4Decompress115(data []byte) ([]byte, error) {
	var out []byte
	for len(data) > 0 {
		if len(data) < 2 {
			return nil, fmt.Errorf("115 LZ4 响应长度无效")
		}
		blockLen := int(data[0]) | int(data[1])<<8
		if blockLen == 0 {
			break
		}
		if len(data) < blockLen+2 {
			return nil, fmt.Errorf("115 LZ4 响应分片长度无效")
		}
		block, err := uploadLZ4BlockDecompress115(data[2 : blockLen+2])
		if err != nil {
			return nil, err
		}
		out = append(out, block...)
		data = data[blockLen+2:]
	}
	return out, nil
}

func uploadLZ4BlockDecompress115(src []byte) ([]byte, error) {
	var dst []byte
	for pos := 0; pos < len(src); {
		token := int(src[pos])
		pos++
		litLen := token >> 4
		if litLen == 15 {
			for {
				if pos >= len(src) {
					return nil, fmt.Errorf("115 LZ4 literal 长度无效")
				}
				n := int(src[pos])
				pos++
				litLen += n
				if n != 255 {
					break
				}
			}
		}
		if pos+litLen > len(src) {
			return nil, fmt.Errorf("115 LZ4 literal 越界")
		}
		dst = append(dst, src[pos:pos+litLen]...)
		pos += litLen
		if pos >= len(src) {
			break
		}
		if pos+2 > len(src) {
			return nil, fmt.Errorf("115 LZ4 offset 越界")
		}
		offset := int(src[pos]) | int(src[pos+1])<<8
		pos += 2
		if offset <= 0 || offset > len(dst) {
			return nil, fmt.Errorf("115 LZ4 offset 无效")
		}
		matchLen := token & 0x0f
		if matchLen == 15 {
			for {
				if pos >= len(src) {
					return nil, fmt.Errorf("115 LZ4 match 长度无效")
				}
				n := int(src[pos])
				pos++
				matchLen += n
				if n != 255 {
					break
				}
			}
		}
		matchLen += 4
		matchPos := len(dst) - offset
		for i := 0; i < matchLen; i++ {
			dst = append(dst, dst[matchPos+i])
		}
	}
	return dst, nil
}
