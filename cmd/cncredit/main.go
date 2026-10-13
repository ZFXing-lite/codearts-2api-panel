// cncredit：对 auths 目录全部账号跑每日签到 + 可领取福利活动。
//
// 移植自 codearts2api-cn/cmd/credit 的意图（原实现只查态；这里按父任务要求
// 直接执行签到与领取）。只依赖 go-codearts 内部包。
//
// 用法：
//
//	cncredit [-auth-dir DIR] [-uid UID] [-json]
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go-codearts/internal/cnup"
	"go-codearts/internal/config"
)

// credFile 只需 auth schema 里签到用的子集字段。
type credFile struct {
	UserID          string `json:"user_id"`
	UserName        string `json:"user_name"`
	CloudDragonTok  string `json:"cloud_dragon_token"`
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
	Expiration      string `json:"expiration"`
}

func (f *credFile) signCred() cnup.SignCredential {
	return cnup.SignCredential{
		AccessKeyID:     f.AccessKeyID,
		SecretAccessKey: f.SecretAccessKey,
		SecurityToken:   f.CloudDragonTok,
	}
}

// loadAuths 读取 dir 下全部 codearts-*.json。
func loadAuths(dir string) ([]*credFile, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []*credFile
	for _, e := range ents {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, "codearts-") || !strings.HasSuffix(name, ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return nil, err
		}
		var f credFile
		if err := json.Unmarshal(raw, &f); err != nil {
			return nil, fmt.Errorf("parse %s: %w", name, err)
		}
		if f.UserID == "" {
			continue
		}
		out = append(out, &f)
	}
	return out, nil
}

func main() {
	cfg := config.Get()
	authDir := flag.String("auth-dir", cfg.CNAuthDir, "auth dir (env CN_AUTH_DIR)")
	uid := flag.String("uid", "", "only this account user_id")
	jsonOut := flag.Bool("json", false, "raw JSON output")
	flag.Parse()

	auths, err := loadAuths(*authDir)
	if err != nil {
		log.Fatalf("load auths: %v", err)
	}
	if len(auths) == 0 {
		log.Fatalf("no accounts in %s", *authDir)
	}

	c := cnup.New(60 * time.Second)
	ctx := context.Background()

	type row struct {
		UserID    string                   `json:"user_id"`
		UserName  string                   `json:"user_name"`
		OK        bool                     `json:"ok"`
		Credits   float64                  `json:"credits,omitempty"`
		Message   string                   `json:"message"`
		Campaigns []cnup.CreditClaimResult `json:"campaigns,omitempty"`
		Benefit   string                   `json:"benefit,omitempty"`
		Error     string                   `json:"error,omitempty"`
	}
	var rows []row
	for _, a := range auths {
		if *uid != "" && a.UserID != *uid {
			continue
		}
		r := row{UserID: a.UserID, UserName: a.UserName}
		res, err := c.Checkin(ctx, a.signCred())
		if err != nil {
			r.Error = err.Error()
		} else {
			r.OK = res.OK
			r.Credits = res.Credits
			r.Message = res.Message
			r.Campaigns = res.Campaigns
		}
		// 限时福利领取（幂等），失败不致命。
		if err := c.ClaimBenefit(a.signCred()); err != nil {
			r.Benefit = "claim error: " + err.Error()
		} else {
			r.Benefit = "ok"
		}
		rows = append(rows, r)
	}

	if *jsonOut {
		raw, _ := json.MarshalIndent(rows, "", "  ")
		fmt.Println(string(raw))
		return
	}
	for _, r := range rows {
		if r.Error != "" {
			fmt.Printf("%s (%s): checkin ERROR %s benefit=%s\n", r.UserID, r.UserName, r.Error, r.Benefit)
			continue
		}
		fmt.Printf("%s (%s): ok=%v +%g credits msg=%q benefit=%s\n",
			r.UserID, r.UserName, r.OK, r.Credits, r.Message, r.Benefit)
		for _, cm := range r.Campaigns {
			fmt.Printf("  campaign %d %s ok=%v status=%s +%g %s\n",
				cm.CampaignID, cm.Title, cm.OK, cm.Status, cm.Credits, cm.Message)
		}
	}
}
