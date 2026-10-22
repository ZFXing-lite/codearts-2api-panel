package cnup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ErrBillingNotConfigured 表示没有配置计费余额查询。
//
// 华为 CodeArts 的积分余额没有公开固定的查询清单：端点是华为云通用计量
// 计费接口，产品码（ProductCode）随产品而变。没有产品码时按错值去查，
// 只会拿到空结果或别人的额度口径——那比"没查"更糟。所以这里要求显式配置，
// 未配置就老老实实返回这个错误，由面板显示"未启用"。
var ErrBillingNotConfigured = errors.New("cnup: billing balance query not configured")

// BillingConfig 计费余额查询配置。
type BillingConfig struct {
	// BaseURL 计费服务基址，例如 https://bss.myhuaweicloud.com
	BaseURL string
	// ProductCode 产品码，例如 p_xxxxxxxx。取错会查到别的产品额度。
	ProductCode string
	// MeterPath 采集路径，缺省用华为云通用 /v2/billing/meter/get-user-resource。
	MeterPath string
}

// Configured 报告该配置是否可用于查询。
func (b BillingConfig) Configured() bool {
	return strings.TrimSpace(b.BaseURL) != "" && strings.TrimSpace(b.ProductCode) != ""
}

// CreditPackage 单个套餐/奖励包的额度明细。
type CreditPackage struct {
	Name    string `json:"name"`
	Remain  int64  `json:"remain"`
	Size    int64  `json:"size"`
	EndTime string `json:"end_time,omitempty"`
}

// CreditBalance 一个账号的积分余额。
//
// Remain 的取数口径：套餐按周期额度优先（CycleCapacityRemain），
// 没有周期额度才用总量额度（CapacityRemain）；负值夹到 0。
type CreditBalance struct {
	Remain    int64           `json:"remain"`
	Packages  []CreditPackage `json:"packages,omitempty"`
	FetchedAt time.Time       `json:"fetched_at"`
}

const defaultMeterPath = "/v2/billing/meter/get-user-resource"

// FetchCreditBalance 查询某账号的积分余额。
//
// 只读，不改任何上游状态。上游 404 通常意味着"这个产品不走该计量接口"，
// 会原样带回来，便于运维区分「配置错」和「账号无额度」。
func (c *Client) FetchCreditBalance(ctx context.Context, cfg BillingConfig, cred SignCredential) (*CreditBalance, error) {
	if !cfg.Configured() {
		return nil, ErrBillingNotConfigured
	}
	path := strings.TrimSpace(cfg.MeterPath)
	if path == "" {
		path = defaultMeterPath
	}
	now := time.Now()
	body, err := json.Marshal(map[string]any{
		"PageNumber":               1,
		"PageSize":                 100,
		"ProductCode":              cfg.ProductCode,
		"Status":                   []int{0, 3},
		"PackageEndTimeRangeBegin": now.Format(packageEndLayout),
		"PackageEndTimeRangeEnd":   now.Add(365 * 101 * 24 * time.Hour).Format(packageEndLayout),
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(cfg.BaseURL, "/")+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	signRequest(req, body, cred)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read billing body: %w", err)
	}
	if resp.StatusCode >= 400 {
		return nil, &ApiError{
			Code: resp.StatusCode, Status: resp.StatusCode, Path: path,
			Message: truncateStr(string(raw), 300),
		}
	}
	var out struct {
		Code    *int   `json:"code"`
		Message string `json:"message"`
		Data    struct {
			Response struct {
				Data struct {
					Accounts []struct {
						PackageName         string `json:"PackageName"`
						PackageEndTime      string `json:"PackageEndTime"`
						CapacitySize        int64  `json:"CapacitySize"`
						CapacityRemain      int64  `json:"CapacityRemain"`
						CycleCapacitySize   int64  `json:"CycleCapacitySize"`
						CycleCapacityRemain int64  `json:"CycleCapacityRemain"`
					} `json:"Accounts"`
				} `json:"Data"`
			} `json:"Response"`
		} `json:"Data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("parse billing balance: %w", err)
	}
	if out.Code != nil && *out.Code != 0 {
		return nil, &ApiError{Code: *out.Code, Status: http.StatusBadRequest, Path: path, Message: out.Message}
	}
	bal := &CreditBalance{FetchedAt: now}
	for _, a := range out.Data.Response.Data.Accounts {
		remain := a.CapacityRemain
		if a.CycleCapacitySize > 0 {
			remain = a.CycleCapacityRemain
		}
		if remain < 0 {
			remain = 0
		}
		bal.Remain += remain
		bal.Packages = append(bal.Packages, CreditPackage{
			Name:    a.PackageName,
			Remain:  remain,
			Size:    a.CapacitySize,
			EndTime: a.PackageEndTime,
		})
	}
	return bal, nil
}

// packageEndLayout 上游 PackageEndTime 的时间格式。
const packageEndLayout = "2006-01-02 15:04:05"
