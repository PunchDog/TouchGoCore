package util

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"os"
	"time"
)

// HTTPGet 发送 GET 请求，返回响应体字节切片
func HTTPGet(uri string) ([]byte, error) {
	response, err := http.Get(uri)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("http get error: uri=%v, statusCode=%v", uri, response.StatusCode)
	}
	return io.ReadAll(response.Body)
}

// HttpPost 发送 POST 表单请求
func HttpPost(uri string, data string) ([]byte, error) {
	response, err := http.Post(uri, "application/x-www-form-urlencoded;charset=utf-8", bytes.NewBufferString(data))
	if err != nil {
		log.Println(err)
		return nil, err
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("http post error: uri=%v, statusCode=%v", uri, response.StatusCode)
	}
	return io.ReadAll(response.Body)
}

// HttpPostByContentType 发送指定 Content-Type 的 POST 请求，返回响应体字符串
func HttpPostByContentType(url string, data interface{}, contentType string) (string, error) {
	jsonStr, err := json.Marshal(data)
	if err != nil {
		return "", fmt.Errorf("marshal request body: %w", err)
	}

	req, err := http.NewRequest("POST", url, bytes.NewBuffer(jsonStr))
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("charset", "UTF-8")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("do request: %w", err)
	}
	defer resp.Body.Close()

	result, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read response body: %w", err)
	}
	return string(result), nil
}

// PostJSONAuth 带 Basic Auth 的 POST JSON 请求
func PostJSONAuth(url string, data interface{}, user string, password string) ([]byte, error) {
	jsonStr, err := json.Marshal(data)
	if err != nil {
		return nil, fmt.Errorf("marshal request body: %w", err)
	}

	req, err := http.NewRequest("POST", url, bytes.NewBuffer(jsonStr))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("charset", "UTF-8")
	req.SetBasicAuth(user, password)

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("do request: %w", err)
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

// PostJSON 发送 JSON POST 请求
func PostJSON(uri string, obj interface{}) ([]byte, error) {
	jsonData, err := json.Marshal(obj)
	if err != nil {
		return nil, err
	}
	response, err := http.Post(uri, "application/json;charset=utf-8", bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("http post error: uri=%v, statusCode=%v", uri, response.StatusCode)
	}
	return io.ReadAll(response.Body)
}

// PostFile 上传单个文件
func PostFile(fieldname, filename, uri string) ([]byte, error) {
	fields := []MultipartFormField{
		{
			IsFile:    true,
			Fieldname: fieldname,
			Filename:  filename,
		},
	}
	return PostMultipartForm(fields, uri)
}

// MultipartFormField 保存文件或其他字段信息
type MultipartFormField struct {
	IsFile    bool
	Fieldname string
	Value     []byte
	Filename  string
}

// PostMultipartForm 上传文件或其他多个字段
func PostMultipartForm(fields []MultipartFormField, uri string) ([]byte, error) {
	bodyBuf := &bytes.Buffer{}
	bodyWriter := multipart.NewWriter(bodyBuf)

	for _, field := range fields {
		if field.IsFile {
			fileWriter, err := bodyWriter.CreateFormFile(field.Fieldname, field.Filename)
			if err != nil {
				return nil, fmt.Errorf("error writing to buffer: %w", err)
			}
			fh, err := os.Open(field.Filename)
			if err != nil {
				return nil, fmt.Errorf("error opening file: %w", err)
			}
			defer fh.Close()

			if _, err = io.Copy(fileWriter, fh); err != nil {
				return nil, err
			}
		} else {
			partWriter, err := bodyWriter.CreateFormField(field.Fieldname)
			if err != nil {
				return nil, err
			}
			if _, err = io.Copy(partWriter, bytes.NewReader(field.Value)); err != nil {
				return nil, err
			}
		}
	}

	contentType := bodyWriter.FormDataContentType()
	bodyWriter.Close()

	resp, err := http.Post(uri, contentType, bodyBuf)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("http post error: uri=%v, statusCode=%v", uri, resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}
