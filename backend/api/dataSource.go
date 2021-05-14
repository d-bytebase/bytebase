package api

import "context"

type DataSourceType string

const (
	Admin DataSourceType = "ADMIN"
	RW    DataSourceType = "RW"
	RO    DataSourceType = "RO"
)

func (e DataSourceType) String() string {
	switch e {
	case Admin:
		return "ADMIN"
	case RW:
		return "RW"
	case RO:
		return "RO"
	}
	return ""
}

type DataSource struct {
	ID int `jsonapi:"primary,dataSource"`

	// Standard fields
	CreatorId   int   `jsonapi:"attr,creatorId"`
	CreatedTs   int64 `jsonapi:"attr,createdTs"`
	UpdaterId   int   `jsonapi:"attr,updaterId"`
	UpdatedTs   int64 `jsonapi:"attr,updatedTs"`
	WorkspaceId int

	// Related fields
	Instance *ResourceObject `jsonapi:"relation,instance"`
	Database *ResourceObject `jsonapi:"relation,database"`

	// Domain specific fields
	Name     string         `jsonapi:"attr,name"`
	Type     DataSourceType `jsonapi:"attr,type"`
	Username string         `jsonapi:"attr,username"`
	Password string         `jsonapi:"attr,password"`
}

type DataSourceCreate struct {
	// Standard fields
	// Value is assigned from the jwt subject field passed by the client.
	CreatorId   int
	WorkspaceId int

	// Related fields
	InstanceId int
	DatabaseId int

	// Domain specific fields
	Name     string         `jsonapi:"attr,name"`
	Type     DataSourceType `jsonapi:"attr,type"`
	Username string         `jsonapi:"attr,username"`
	Password string         `jsonapi:"attr,password"`
}

type DataSourceFind struct {
	// Standard fields
	WorkspaceId *int

	// Related fields
	InstanceId *int
	DatabaseId *int

	// Domain specific fields
	Type *DataSourceType
}

type DataSourcePatch struct {
	ID int `jsonapi:"primary,dataSourcePatch"`

	// Standard fields
	// Value is assigned from the jwt subject field passed by the client.
	UpdaterId   int
	WorkspaceId int

	// Domain specific fields
	Username *string `jsonapi:"attr,username"`
	Password *string `jsonapi:"attr,password"`
}

type DataSourceService interface {
	CreateDataSource(ctx context.Context, create *DataSourceCreate) (*DataSource, error)
	FindDataSourceList(ctx context.Context, find *DataSourceFind) ([]*DataSource, error)
	FindDataSource(ctx context.Context, find *DataSourceFind) (*DataSource, error)
	PatchDataSource(ctx context.Context, patch *DataSourcePatch) (*DataSource, error)
}