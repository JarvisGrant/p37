// Package acceptance 存放 P36 真机验收用的测试。
//
// 真正的测试文件带 //go:build p36acceptance，日常 go test ./... 不会运行它；
// GitHub Actions 会用：
//
//	go test -c -tags p36acceptance ./packaging/p36/acceptance
//
// 构建后在 P36/Ubuntu runner 上以 p36 用户执行。
package acceptance
