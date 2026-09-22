package util

import (
	"os"
	"path/filepath"
	"strings"
)

// 获取路径下文件列表
func GetPathFile(path string, filter []string) []string {
	// 判断所给路径是否为文件夹
	isDir := func(path string) bool {
		s, err := os.Stat(path)
		if err != nil {
			return false
		}
		return s.IsDir()
	}

	//获取当前目录下的文件或目录名(包含路径)
	pathlen := len(path)
	if path[pathlen-1] != '/' {
		path = path + "/"
	}
	//获取当前目录下的文件或目录名(包含路径)
	filepathNames, err := filepath.Glob(path + "*")
	if err != nil {
		return nil
	}

	strRetList := []string{}

	//遍历路径,但是会给文件夹优先级放后
	for _, filenamepath := range filepathNames {
		if isDir(filenamepath) {
			list := GetPathFile(filenamepath, filter)
			if len(list) > 0 {
				strRetList = append(strRetList, list...)
			}
		} else {
			//过滤带关键字的
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
