echo - cleanup
del /f ./message/*.go

set DIR=%~dp0
cd %DIR%proto
..\protoc.exe --plugin=protoc-gen-go="..\protoc-gen-go.exe" --go_out=../message/ *.proto

rem pause