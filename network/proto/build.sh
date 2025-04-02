#!/bin/bash
# 生成proto文件
#循环编译文件夹下所有proto文件
for file in `ls *.proto`
do
    #获取文件名
    filename=${file##*/}
    #打印文件名
    echo "正在转换文件"$filename
    #编译proto文件
    protoc --go_out=../../../ $file
done