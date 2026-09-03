//go:build ignore

package db

import (
	"time"

	"gorm.io/gorm"
)

// User 用户模型示例
// 使用示例:
//   db.Create(&User{Name: "John", Email: "john@example.com"})
//   var user User; db.First(&user, 1)
//   db.Where("age > ?", 18).Find(&users)
type User struct {
	ID        uint           `gorm:"primaryKey" json:"id"`
	Name      string         `gorm:"size:100;not null" json:"name"`
	Email     string         `gorm:"size:100;uniqueIndex;not null" json:"email"`
	Age       int            `gorm:"default:0" json:"age"`
	Status    string         `gorm:"size:20;default:'active';index" json:"status"`
	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
	DeletedAt gorm.DeletedAt `gorm:"index" json:"-"`
	Orders    []Order        `gorm:"foreignKey:UserID" json:"orders,omitempty"`
}

// TableName 指定表名（可选，默认会使用结构体名的复数形式）
func (User) TableName() string {
	return "users"
}

// BeforeCreate GORM 钩子：创建前
func (u *User) BeforeCreate(tx *gorm.DB) error {
	if u.Status == "" {
		u.Status = "active"
	}
	return nil
}

// Order 订单模型示例
type Order struct {
	ID        uint           `gorm:"primaryKey" json:"id"`
	UserID    uint           `gorm:"not null;index" json:"user_id"`
	OrderNo   string         `gorm:"size:50;uniqueIndex;not null" json:"order_no"`
	Amount    float64        `gorm:"type:decimal(10,2);not null" json:"amount"`
	Status    string         `gorm:"size:20;default:'pending';index" json:"status"`
	PaidAt    *time.Time     `json:"paid_at,omitempty"`
	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
	DeletedAt gorm.DeletedAt `gorm:"index" json:"-"`
	User      User           `gorm:"foreignKey:UserID" json:"user,omitempty"`
}

// Product 商品模型示例
type Product struct {
	ID        uint           `gorm:"primaryKey" json:"id"`
	Name      string         `gorm:"size:200;not null" json:"name"`
	Price     float64        `gorm:"type:decimal(10,2);not null" json:"price"`
	Stock     int            `gorm:"default:0" json:"stock"`
	Category  string         `gorm:"size:50;index" json:"category"`
	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
	DeletedAt gorm.DeletedAt `gorm:"index" json:"-"`
}

// Category 分类模型示例
type Category struct {
	ID        uint           `gorm:"primaryKey" json:"id"`
	Name      string         `gorm:"size:100;uniqueIndex;not null" json:"name"`
	ParentID  *uint          `gorm:"index" json:"parent_id,omitempty"`
	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
	Children  []Category     `gorm:"foreignKey:ParentID" json:"children,omitempty"`
	Products  []Product      `gorm:"foreignKey:CategoryID" json:"products,omitempty"`
}

// Log 日志模型示例
type Log struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	Level     string    `gorm:"size:10;index" json:"level"`
	Message   string    `gorm:"type:text" json:"message"`
	Context   string    `gorm:"type:text" json:"context,omitempty"`
	CreatedAt time.Time `gorm:"index" json:"created_at"`
}

// Setting 配置模型示例
type Setting struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	Key       string    `gorm:"size:100;uniqueIndex;not null" json:"key"`
	Value     string    `gorm:"type:text" json:"value"`
	Type      string    `gorm:"size:20;default:'string'" json:"type"` // string, int, bool, json
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Tag 标签模型示例（多对多关系）
type Tag struct {
	ID        uint         `gorm:"primaryKey" json:"id"`
	Name      string       `gorm:"size:50;uniqueIndex;not null" json:"name"`
	CreatedAt time.Time    `json:"created_at"`
	UpdatedAt time.Time    `json:"updated_at"`
	Products  []ProductTag `gorm:"foreignKey:TagID" json:"products,omitempty"`
}

// ProductTag 商品标签关联表（多对多）
type ProductTag struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	ProductID uint      `gorm:"not null;index" json:"product_id"`
	TagID     uint      `gorm:"not null;index" json:"tag_id"`
	CreatedAt time.Time `json:"created_at"`
}

// Profile 用户资料模型（一对一关系）
type Profile struct {
	ID        uint           `gorm:"primaryKey" json:"id"`
	UserID    uint           `gorm:"uniqueIndex;not null" json:"user_id"`
	Avatar    string         `gorm:"size:255" json:"avatar,omitempty"`
	Bio       string         `gorm:"type:text" json:"bio,omitempty"`
	Phone     string         `gorm:"size:20" json:"phone,omitempty"`
	Address   string         `gorm:"type:text" json:"address,omitempty"`
	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
	DeletedAt gorm.DeletedAt `gorm:"index" json:"-"`
	User      User           `gorm:"foreignKey:UserID" json:"user,omitempty"`
}

// OrderItem 订单项模型（一对多关系）
type OrderItem struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	OrderID   uint      `gorm:"not null;index" json:"order_id"`
	ProductID uint      `gorm:"not null;index" json:"product_id"`
	Quantity  int       `gorm:"not null" json:"quantity"`
	Price     float64   `gorm:"type:decimal(10,2);not null" json:"price"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Order     Order     `gorm:"foreignKey:OrderID" json:"order,omitempty"`
	Product   Product   `gorm:"foreignKey:ProductID" json:"product,omitempty"`
}

// Article 文章模型示例
type Article struct {
	ID        uint           `gorm:"primaryKey" json:"id"`
	Title     string         `gorm:"size:200;not null" json:"title"`
	Content   string         `gorm:"type:longtext" json:"content"`
	Summary   string         `gorm:"type:text" json:"summary,omitempty"`
	AuthorID  uint           `gorm:"not null;index" json:"author_id"`
	Status    string         `gorm:"size:20;default:'draft';index" json:"status"` // draft, published, archived
	ViewCount int            `gorm:"default:0" json:"view_count"`
	PublishedAt *time.Time   `json:"published_at,omitempty"`
	CreatedAt time.Time      `gorm:"index" json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
	DeletedAt gorm.DeletedAt `gorm:"index" json:"-"`
	Author    User           `gorm:"foreignKey:AuthorID" json:"author,omitempty"`
	Tags      []Tag          `gorm:"many2many:article_tags;" json:"tags,omitempty"`
}

// Session 会话模型示例
type Session struct {
	ID        string    `gorm:"primaryKey;size:100" json:"id"`
	UserID    uint      `gorm:"not null;index" json:"user_id"`
	IP        string    `gorm:"size:50" json:"ip,omitempty"`
	UserAgent string    `gorm:"size:255" json:"user_agent,omitempty"`
	ExpiresAt time.Time `gorm:"index" json:"expires_at"`
	CreatedAt time.Time `json:"created_at"`
	User      User      `gorm:"foreignKey:UserID" json:"user,omitempty"`
}

// Payment 支付记录模型示例
type Payment struct {
	ID         uint      `gorm:"primaryKey" json:"id"`
	OrderID    uint      `gorm:"not null;index" json:"order_id"`
	Amount     float64   `gorm:"type:decimal(10,2);not null" json:"amount"`
	Method     string    `gorm:"size:20;not null" json:"method"` // alipay, wechat, credit_card
	Status     string    `gorm:"size:20;default:'pending';index" json:"status"` // pending, success, failed
	TransactionID string `gorm:"size:100;index" json:"transaction_id,omitempty"`
	PaidAt     *time.Time `json:"paid_at,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
	Order      Order     `gorm:"foreignKey:OrderID" json:"order,omitempty"`
}

// Notification 通知模型示例
type Notification struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	UserID    uint      `gorm:"not null;index" json:"user_id"`
	Type      string    `gorm:"size:50;not null;index" json:"type"`
	Title     string    `gorm:"size:200;not null" json:"title"`
	Content   string    `gorm:"type:text" json:"content"`
	Data      string    `gorm:"type:json" json:"data,omitempty"` // JSON 格式的额外数据
	IsRead    bool      `gorm:"default:false;index" json:"is_read"`
	ReadAt    *time.Time `json:"read_at,omitempty"`
	CreatedAt time.Time `gorm:"index" json:"created_at"`
	User      User      `gorm:"foreignKey:UserID" json:"user,omitempty"`
}

// Comment 评论模型示例
type Comment struct {
	ID        uint           `gorm:"primaryKey" json:"id"`
	TargetType string       `gorm:"size:50;not null;index" json:"target_type"` // article, product
	TargetID   uint         `gorm:"not null;index" json:"target_id"`
	UserID     uint         `gorm:"not null;index" json:"user_id"`
	ParentID   *uint        `gorm:"index" json:"parent_id,omitempty"`
	Content    string       `gorm:"type:text;not null" json:"content"`
	IP         string       `gorm:"size:50" json:"ip,omitempty"`
	CreatedAt  time.Time    `gorm:"index" json:"created_at"`
	UpdatedAt  time.Time    `json:"updated_at"`
	DeletedAt  gorm.DeletedAt `gorm:"index" json:"-"`
	User       User         `gorm:"foreignKey:UserID" json:"user,omitempty"`
	Replies    []Comment    `gorm:"foreignKey:ParentID" json:"replies,omitempty"`
}

// Attachment 附件模型示例
type Attachment struct {
	ID          uint           `gorm:"primaryKey" json:"id"`
	FileName    string         `gorm:"size:255;not null" json:"file_name"`
	FilePath    string         `gorm:"size:500;not null" json:"file_path"`
	FileSize    int64          `gorm:"not null" json:"file_size"`
	FileType    string         `gorm:"size:50;index" json:"file_type"` // image, video, document
	MimeType    string         `gorm:"size:100" json:"mime_type"`
	UploaderID  uint           `gorm:"not null;index" json:"uploader_id"`
	CreatedAt   time.Time      `json:"created_at"`
	DeletedAt   gorm.DeletedAt `gorm:"index" json:"-"`
	Uploader    User           `gorm:"foreignKey:UploaderID" json:"uploader,omitempty"`
}

// Permission 权限模型示例（RBAC）
type Permission struct {
	ID          uint         `gorm:"primaryKey" json:"id"`
	Name        string       `gorm:"size:100;uniqueIndex;not null" json:"name"`
	Code        string       `gorm:"size:100;uniqueIndex;not null" json:"code"`
	Description string       `gorm:"type:text" json:"description,omitempty"`
	CreatedAt   time.Time    `json:"created_at"`
	UpdatedAt   time.Time    `json:"updated_at"`
	Roles       []Role       `gorm:"many2many:role_permissions;" json:"roles,omitempty"`
}

// Role 角色模型示例（RBAC）
type Role struct {
	ID          uint         `gorm:"primaryKey" json:"id"`
	Name        string       `gorm:"size:50;uniqueIndex;not null" json:"name"`
	Code        string       `gorm:"size:50;uniqueIndex;not null" json:"code"`
	Description string       `gorm:"type:text" json:"description,omitempty"`
	CreatedAt   time.Time    `json:"created_at"`
	UpdatedAt   time.Time    `json:"updated_at"`
	Permissions []Permission `gorm:"many2many:role_permissions;" json:"permissions,omitempty"`
	Users       []User       `gorm:"many2many:user_roles;" json:"users,omitempty"`
}

// GORM 模型标签说明：
/*
常用标签：
- `primaryKey` - 主键
- `autoIncrement` - 自增
- `not null` - 非空
- `uniqueIndex` - 唯一索引
- `index` - 普通索引
- `size:n` - 字段长度
- `type:type_name` - 数据库类型（如 varchar, decimal, text, longtext）
- `default:value` - 默认值
- `comment:comment_text` - 字段注释
- `embedded` - 嵌入结构体
- `embeddedPrefix:prefix` - 嵌入前缀
- `foreignKey:key` - 外键
- `references:key` - 引用字段
- `many2many:table_name` - 多对多关联表
- `many2many:table_name;foreignKey:key;joinForeignKey:key;joinReferences:key;references:key` - 多对多详细配置

关系类型：
- 一对一：`gorm:"foreignKey:UserID"`
- 一对多：`gorm:"foreignKey:UserID"`
- 多对多：`gorm:"many2many:table_name;"`
- 多态：`gorm:"polymorphic:Type;polymorphicValue:ModelName"`

时间相关：
- `autoCreateTime` - 自动创建时间
- `autoUpdateTime` - 自动更新时间
- `ignore` - 忽略该字段

JSON 标签（用于序列化）：
- `json:"field_name"` - JSON 字段名
- `json:"field_name,omitempty"` - 可选字段
- `json:"-"` - 不序列化

软删除：
- `gorm.DeletedAt` - 软删除字段
- `gorm:"index"` - DeletedAt 需要索引

示例：
type User struct {
    ID        uint           `gorm:"primaryKey"`
    Name      string         `gorm:"size:100;not null;index"`
    Email     string         `gorm:"size:100;uniqueIndex;not null"`
    Age       int            `gorm:"default:0"`
    Status    string         `gorm:"size:20;default:'active';index"`
    CreatedAt time.Time      `gorm:"autoCreateTime"`
    UpdatedAt time.Time      `gorm:"autoUpdateTime"`
    DeletedAt gorm.DeletedAt `gorm:"index"`
}
*/
