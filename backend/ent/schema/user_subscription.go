package schema

import (
	"time"

	"github.com/Wei-Shaw/sub2api/ent/schema/mixins"
	"github.com/Wei-Shaw/sub2api/internal/domain"

	"entgo.io/ent"
	"entgo.io/ent/dialect"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// UserSubscription 是「个人额度钱包」，不绑定任何分组。
//
// 产品定案（2026-10-03 拍板）：
//   - 订阅不授予分组准入，只管理额度；Key 绑在哪个分组就按哪个分组的倍率计费，
//     消耗的是这份钱包里的钱。
//   - 额度模型为单一总额池（对齐 new-api 的 amount_total / amount_used）：
//     一次性总额，花完为止，有效期到期后剩余作废，不随日/周/月滚动重置。
//   - 一个用户可同时持有多份订阅（同一套餐重复购买 = 新增一份独立订阅），
//     消耗顺序为「先到期先消耗」，单笔费用可跨订阅拆分。
//
// 历史包袱：本表曾有 group_id 槽位（(user,0) 个人 / (user,G) 分组专属）与
// 日/周/月三套 limit/usage/window 列，随「订阅制分组」机制一并废弃，见迁移 246。
type UserSubscription struct {
	ent.Schema
}

func (UserSubscription) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "user_subscriptions"},
	}
}

func (UserSubscription) Mixin() []ent.Mixin {
	return []ent.Mixin{
		mixins.TimeMixin{},
		mixins.SoftDeleteMixin{},
	}
}

func (UserSubscription) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("user_id"),

		// 来源套餐（可空）：管理员手工发放的订阅没有套餐，因此必须 Optional+Nillable。
		// 仅用于展示与追溯，额度以本行 total_limit_usd 的快照为准，
		// 套餐后续改价/改额度不影响已发放的订阅。
		field.Int64("plan_id").
			Optional().
			Nillable(),

		field.Time("starts_at").
			SchemaType(map[string]string{dialect.Postgres: "timestamptz"}),
		field.Time("expires_at").
			SchemaType(map[string]string{dialect.Postgres: "timestamptz"}),
		field.String("status").
			MaxLen(20).
			Default(domain.SubscriptionStatusActive),

		// 总额池额度（USD）：NULL 或 <=0 表示不限额。
		field.Float("total_limit_usd").
			Optional().
			Nillable().
			SchemaType(map[string]string{dialect.Postgres: "decimal(20,8)"}),
		// 已消耗金额（USD）：随请求原子累加，订阅作废/到期不回滚。
		field.Float("total_usage_usd").
			SchemaType(map[string]string{dialect.Postgres: "decimal(20,10)"}).
			Default(0),

		field.Int64("assigned_by").
			Optional().
			Nillable(),
		field.Time("assigned_at").
			Default(time.Now).
			SchemaType(map[string]string{dialect.Postgres: "timestamptz"}),
		field.String("notes").
			Optional().
			Nillable().
			SchemaType(map[string]string{dialect.Postgres: "text"}),
	}
}

func (UserSubscription) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("user", User.Type).
			Ref("subscriptions").
			Field("user_id").
			Unique().
			Required(),
		edge.From("plan", SubscriptionPlan.Type).
			Ref("user_subscriptions").
			Field("plan_id").
			Unique(),
		edge.From("assigned_by_user", User.Type).
			Ref("assigned_subscriptions").
			Field("assigned_by").
			Unique(),
		edge.To("usage_logs", UsageLog.Type),
	}
}

func (UserSubscription) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("user_id"),
		index.Fields("plan_id"),
		index.Fields("status"),
		index.Fields("expires_at"),
		// 网关热路径：按用户取活跃钱包（线上由 SQL 迁移创建部分索引）。
		index.Fields("user_id", "status", "expires_at"),
		index.Fields("assigned_by"),
		index.Fields("deleted_at"),
	}
}
