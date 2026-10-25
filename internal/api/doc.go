// Package api 提供 HTTP 层：路由、中间件、请求/响应编解码与 SSE 端点。
//
// 依赖方向：cmd/server → api → session / agent / sse。
// api 不实现业务规则，只做协议适配与状态编排。
package api
