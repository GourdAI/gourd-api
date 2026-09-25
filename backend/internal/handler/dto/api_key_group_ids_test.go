package dto

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// api_key_group_ids_test.go 验证「密钥 JSON 新增 group_ids」的冻结契约：
//   - group_ids 始终存在（无绑定时为空数组，而非字段缺失）；
//   - 多分组 key 按主分组优先输出；
//   - 单分组 key 退化为单元素列表（向后兼容：老前端忽略该字段仍可工作）；
//   - group_id / group 继续返回主分组，行为不变。

func TestAPIKeyFromServiceGroupIDsContract(t *testing.T) {
	t.Run("multi-group key exposes ordered group_ids and keeps primary group", func(t *testing.T) {
		primaryID := int64(10)
		secondaryID := int64(20)
		primary := &service.Group{ID: primaryID, Name: "primary", Platform: service.PlatformOpenAI}
		secondary := &service.Group{ID: secondaryID, Name: "secondary", Platform: service.PlatformOpenAI}

		out := APIKeyFromService(&service.APIKey{
			ID:       1,
			GroupID:  &primaryID,
			Group:    primary,
			GroupIDs: []int64{primaryID, secondaryID},
			Groups:   []*service.Group{primary, secondary},
		})

		require.NotNil(t, out)
		require.Equal(t, []int64{primaryID, secondaryID}, out.GroupIDs)
		require.NotNil(t, out.GroupID, "group_id 必须继续返回主分组（兼容）")
		require.Equal(t, primaryID, *out.GroupID)
		require.NotNil(t, out.Group, "group 必须继续返回主分组对象（兼容）")
		require.Equal(t, primaryID, out.Group.ID)

		// JSON 序列化后字段必须存在（前端按 always-present 处理）。
		raw, err := json.Marshal(out)
		require.NoError(t, err)
		var decoded map[string]any
		require.NoError(t, json.Unmarshal(raw, &decoded))
		require.Contains(t, decoded, "group_ids", "group_ids 字段必须始终存在于密钥 JSON")
	})

	t.Run("single-group key degrades to one-element list", func(t *testing.T) {
		groupID := int64(7)
		out := APIKeyFromService(&service.APIKey{
			ID:      2,
			GroupID: &groupID,
			Group:   &service.Group{ID: groupID, Platform: service.PlatformOpenAI},
		})
		require.Equal(t, []int64{groupID}, out.GroupIDs)
		require.Equal(t, groupID, *out.GroupID)
	})

	t.Run("unbound key yields empty (non-nil) list", func(t *testing.T) {
		out := APIKeyFromService(&service.APIKey{ID: 3})
		require.NotNil(t, out.GroupIDs, "group_ids 始终存在，无绑定时为空数组")
		require.Empty(t, out.GroupIDs)
		require.Nil(t, out.GroupID, "无绑定时 group_id 仍为 null")
	})

	t.Run("nil api key yields nil dto", func(t *testing.T) {
		require.Nil(t, APIKeyFromService(nil))
	})
}
