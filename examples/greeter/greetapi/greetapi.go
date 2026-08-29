// Package greetapi 顶替合约仓库生成物（psp-contracts/go/greetv1）的位置：
// 接口 + DTO + 错误码，是 greet 域对外的全部可见类型。
//
// 真实项目中本包由 OpenAPI 生成并以独立 module 发版，提供方（greet）与
// 消费方（gateway）都只 import 它——双方互相看不到实现，跨域调用只经
// 契约类型（DESIGN §1 铁律 2）。
package greetapi

import "context"

// Service 是 greet 域的契约接口。本地实现与远程 client 实现同一个接口，
// 调用方无从（也不需要）分辨对端形态。
type Service interface {
	Greet(ctx context.Context, req GreetRequest) (GreetReply, error)
}

// GreetRequest 传值、可序列化——契约边界禁止传指针（DESIGN §5.3）。
type GreetRequest struct {
	Name string `json:"name"`
	// Lang 是问候语言，空值等价 "en"。
	Lang string `json:"lang,omitempty"`
}

type GreetReply struct {
	Message string `json:"message"`
}

// CodeUnsupportedLang 顶替 codes.gen.go 生成的错误码常量：错误身份 = 错误码，
// apperr.Is 在单体（原始错误）与微服务（problem+json 重建）两种形态下判定一致。
const CodeUnsupportedLang = "GREET_UNSUPPORTED_LANG"
