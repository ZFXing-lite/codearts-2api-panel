package api

// 面板「模型」页的双渠道目录聚合。
// 国际版模型的权威来源是上游 snap-access 目录 + 各账号已检出 Models；
// 国内码道模型来自 cnup 按账号缓存的检出目录（key = UserID）。
// 两者在 /v1/models 里以 cn: 前缀合流，本接口负责把归属、可用账号数、
// 福利标记等元信息一次性交给面板。

import (
	"net/http"
	"sort"
	"strings"

	"go-codearts/internal/cnup"
	"go-codearts/internal/huawei"
	"go-codearts/internal/store"
)

type modelRow struct {
	ID       string `json:"id"`
	Prefixed string `json:"prefixed,omitempty"` // CN 行带 cn: 前缀形态
	Accounts int    `json:"accounts"`           // 可用账号数（0 = 仅兜底目录）
	Benefit  bool   `json:"benefit,omitempty"`  // CN 限时福利模型
	Fallback bool   `json:"fallback,omitempty"` // 国际兜底目录（别名映射可达）
}

func (s *Server) handleModelsCatalog(w http.ResponseWriter, r *http.Request) {
	intl := map[string]int{}
	fallback := map[string]bool{}
	cn := map[string]int{}
	cnBenefit := map[string]bool{}

	var cnAccounts int
	for _, a := range s.db.SnapshotList() {
		if a == nil {
			continue
		}
		if store.IsCNAccount(a) {
			cnAccounts++
			if infos, ok := cnup.AccountModels(a.UserID); ok {
				for _, m := range infos {
					if m.ID == "" {
						continue
					}
					cn[m.ID]++
					if m.Benefit {
						cnBenefit[m.ID] = true
					}
				}
				continue
			}
			// 目录未发现时回退到账号自身记录的模型名，别让国内分组空白。
			for _, m := range a.Models {
				if m = strings.TrimSpace(m); m != "" {
					cn[m]++
				}
			}
			continue
		}
		for _, m := range a.Models {
			if m != "" {
				intl[m]++
			}
		}
	}

	// 上游远端目录：即使当前没有账号检出，也应展示为可用（0 账号）。
	if s.huaweiModel != nil {
		for _, id := range s.huaweiModel.ModelIDs() {
			if _, ok := intl[id]; !ok {
				intl[id] = 0
			}
		}
	}
	// 内置兜底目录：别名映射（snap-chat → GLM-x）永远可达。
	for _, id := range huawei.FallbackModelIDs {
		fallback[id] = true
		if _, ok := intl[id]; !ok {
			intl[id] = 0
		}
	}

	intlRows := make([]modelRow, 0, len(intl))
	for id, n := range intl {
		intlRows = append(intlRows, modelRow{ID: id, Prefixed: modelPrefixGlobal + id, Accounts: n, Fallback: fallback[id]})
	}
	sort.Slice(intlRows, func(i, j int) bool {
		if intlRows[i].Accounts != intlRows[j].Accounts {
			return intlRows[i].Accounts > intlRows[j].Accounts
		}
		return intlRows[i].ID < intlRows[j].ID
	})

	cnRows := make([]modelRow, 0, len(cn))
	for id, n := range cn {
		cnRows = append(cnRows, modelRow{ID: id, Prefixed: modelPrefixCN + id, Accounts: n, Benefit: cnBenefit[id]})
	}
	sort.Slice(cnRows, func(i, j int) bool {
		if cnRows[i].Accounts != cnRows[j].Accounts {
			return cnRows[i].Accounts > cnRows[j].Accounts
		}
		return cnRows[i].ID < cnRows[j].ID
	})

	writeJSON(w, http.StatusOK, map[string]any{
		"intl":        intlRows,
		"cn":          cnRows,
		"intl_total":  len(intlRows),
		"cn_total":    len(cnRows),
		"cn_accounts": cnAccounts,
		"cn_enabled":  s.cfg.CNEnabled,
	})
}
