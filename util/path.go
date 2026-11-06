package util

import (
	"os"
	"path/filepath"
	"strings"
)

// GetPathFile 递归列出 path 下的文件与目录名（返回含完整路径的字符串）。
//
// filter 非 nil 时按关键字过滤：只有**同时包含全部关键字**的路径才保留（逐个关键字
// 做 Contains 判定，缺一个即跳过）。
func GetPathFile(path string, filter []string) []string {
	// 判断所给路径是否为文件夹
	isDir := func(path string) bool {
		s, err := os.Stat(path)
		if err != nil {
			return false
		}
		return s.IsDir()
	}

	// 目录名末尾不带 "/" 时补一个，后面的 Glob 才能匹配到目录里的条目
	pathlen := len(path)
	if path[pathlen-1] != '/' {
		path = path + "/"
	}
	// Glob 取该目录下的全部条目（不含隐藏文件之外的特殊处理，语义由 pattern 决定）
	filepathNames, err := filepath.Glob(path + "*")
	if err != nil {
		return nil
	}

	strRetList := []string{}

	// 遍历路径：目录递归展开后并入结果，文件按 filter 过滤
	for _, filenamepath := range filepathNames {
		if isDir(filenamepath) {
			list := GetPathFile(filenamepath, filter)
			if len(list) > 0 {
				strRetList = append(strRetList, list...)
			}
		} else {
			// 过滤：必须同时包含全部 filter 关键字才保留
			if filter != nil {
				bContinue := false
				for _, f := range filter {
					if !strings.Contains(filenamepath, f) {
						bContinue = true
						break
					}
				}
				if !bContinue {
					strRetList = append(strRetList, filenamepath)
				}
			} else {
				strRetList = append(strRetList, filenamepath)
			}
		}
	}

	return strRetList
}
