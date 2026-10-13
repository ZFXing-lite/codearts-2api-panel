// cnmodels：拉取 auths 目录各账号的可用模型列表并打印（内置 + 限时福利）。
//
// 移植自 codearts2api-cn/cmd/models，适配 go-codearts/internal/cnup。
//
// 用法：
//
//	cnmodels [-auth-dir DIR] [-uid UID] [-json] [-claim]
package main

import (
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

// credFile 只需 auth schema 里查模型用的子集字段。
type credFile struct {
	UserID          string `json:"user_id"`
	UserName        string `json:"user_name"`
	CloudDragonTok  string `json:"cloud_dragon_token"`
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
}

func (f *credFile) account() *cnup.Account {
	return &cnup.Account{
		UserID:          f.UserID,
		AccessKeyID:     f.AccessKeyID,
		SecretAccessKey: f.SecretAccessKey,
		CloudDragonTok:  f.CloudDragonTok,
	}
}

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
		if f.UserID != "" {
			out = append(out, &f)
		}
	}
	return out, nil
}

func main() {
	cfg := config.Get()
	authDir := flag.String("auth-dir", cfg.CNAuthDir, "auth dir (env CN_AUTH_DIR)")
	uid := flag.String("uid", "", "only this account user_id")
	jsonOut := flag.Bool("json", false, "raw JSON output")
	claim := flag.Bool("claim", false, "先领取限时福利再查询（幂等）")
	flag.Parse()

	auths, err := loadAuths(*authDir)
	if err != nil {
		log.Fatalf("load auths: %v", err)
	}
	if len(auths) == 0 {
		log.Fatalf("no accounts in %s", *authDir)
	}

	c := cnup.New(60 * time.Second)
	c.SetBenefitAutoClaim(*claim)

	for _, a := range auths {
		if *uid != "" && a.UserID != *uid {
			continue
		}
		infos, err := c.FetchModels(a.account())
		if err != nil {
			fmt.Printf("== %s (%s): ERROR %v\n", a.UserID, a.UserName, err)
			continue
		}
		if *jsonOut {
			raw, _ := json.MarshalIndent(map[string]any{
				"user_id":   a.UserID,
				"user_name": a.UserName,
				"models":    infos,
			}, "", "  ")
			fmt.Println(string(raw))
			continue
		}
		benefits := 0
		for _, m := range infos {
			if m.Benefit {
				benefits++
			}
		}
		fmt.Printf("== %s (%s): %d models (%d benefit)\n", a.UserID, a.UserName, len(infos), benefits)
		for _, m := range infos {
			tag := "builtin"
			if m.Benefit {
				tag = "benefit"
			}
			fmt.Printf("  [%-7s] %-28s ctx=%-8d max=%-8d %s\n",
				tag, m.ID, m.ContextWindow, m.MaxTokens, m.Desc)
		}
	}
	if !*claim && !*jsonOut {
		fmt.Fprintln(os.Stderr, "提示：福利模型为空时可加 -claim 领取限时福利后重试。")
	}
}
