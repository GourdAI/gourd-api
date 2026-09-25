//go:build unit

package repository

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// api_key_repo_groups_test.go 覆盖多分组关联表的纯逻辑部分（无 DB 依赖）：
//   - 候选分组归一化（主分组置首、去重、去非正值、其余**保持传入顺序**）；
//   - 与单分组 key 的向后兼容退化。
//
// 顺序契约（回归保护）：归一化绝不可按 ID 升序重排。前端按用户勾选顺序提交，
// group_ids[0] 为主分组；若重排为升序，用户选定的主分组会被静默换成 ID 最小的分组，
// 导致「模型不可知」请求（GET /v1/models 等）回退到错误分组，计费/RPM/日志分组全错。

func TestNormalizeAPIKeyGroupIDs(t *testing.T) {
	primary := int64(30)

	t.Run("primary first then others keep caller order", func(t *testing.T) {
		// 用户勾选顺序 [50,30,10,20]，主分组指定为 30 → 30 置首，其余保持 [50,10,20]。
		got := normalizeAPIKeyGroupIDs([]int64{50, 30, 10, 20}, &primary)
		require.Equal(t, []int64{30, 50, 10, 20}, got, "主分组置首，其余必须保持传入顺序（不得升序重排）")
	})

	t.Run("primary not in set is still prepended, rest keep order", func(t *testing.T) {
		got := normalizeAPIKeyGroupIDs([]int64{50, 10}, &primary)
		require.Equal(t, []int64{30, 50, 10}, got)
	})

	t.Run("nil primary keeps caller order verbatim", func(t *testing.T) {
		// 关键回归：输入本身是乱序的 [7,3,5]，保序后必须原样返回，而不是升序 [3,5,7]。
		got := normalizeAPIKeyGroupIDs([]int64{7, 3, 5}, nil)
		require.Equal(t, []int64{7, 3, 5}, got, "无主分组时必须逐位保持传入顺序")
	})

	t.Run("user-picked primary survives even when it is not the min id", func(t *testing.T) {
		// P0 回归保护：用户把 ID=10 选为主分组、再选 ID=3；主分组必须是 10 而非 3。
		userPicked := int64(10)
		got := normalizeAPIKeyGroupIDs([]int64{10, 3}, &userPicked)
		require.Equal(t, []int64{10, 3}, got, "用户选定的主分组(10)不得被 ID 更小的分组(3)顶替")
	})

	t.Run("non-positive values dropped", func(t *testing.T) {
		got := normalizeAPIKeyGroupIDs([]int64{0, -1, 4, 4}, nil)
		require.Equal(t, []int64{4}, got)
	})

	t.Run("empty input yields empty result", func(t *testing.T) {
		require.Empty(t, normalizeAPIKeyGroupIDs(nil, nil))
	})
}

func TestSingleGroupFallback(t *testing.T) {
	// 单分组 key：退化集合必须恰好等于主分组（回归保护）。
	groupID := int64(9)
	require.Equal(t, []int64{9}, singleGroupFallback(&groupID))

	// 未绑定分组：nil（与历史 group_id=NULL 语义一致）。
	require.Nil(t, singleGroupFallback(nil))

	// 非法主分组：nil。
	zero := int64(0)
	require.Nil(t, singleGroupFallback(&zero))
}
