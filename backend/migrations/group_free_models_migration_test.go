package migrations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// 分组免费模型名单（243）的列形状契约：
// JSONB + NOT NULL + '[]' 默认值 + 重入安全，且必须带列注释。
func TestGroupFreeModelsMigration(t *testing.T) {
	content, err := FS.ReadFile("243_group_free_models.sql")
	require.NoError(t, err)

	sql := strings.Join(strings.Fields(string(content)), " ")
	require.Contains(t, sql,
		"ADD COLUMN IF NOT EXISTS free_models JSONB NOT NULL DEFAULT '[]'::jsonb")
	require.Contains(t, sql, "COMMENT ON COLUMN groups.free_models")
}
