package router

import (
	sql "database/sql"
	// db "mkfst/db"

	gin "github.com/gin-gonic/gin"
)

type MkfstHandler func(
	*gin.Context,
	*sql.DB,
)
