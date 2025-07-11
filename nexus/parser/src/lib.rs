use std::{collections::HashMap, sync::Arc};

use analyzer::{
    CursorEvent, PeerCursorAnalyzer, PeerDDL, PeerDDLAnalyzer, PeerExistanceAnalyzer,
    QueryAssociation, StatementAnalyzer,
};
use async_trait::async_trait;
use catalog::Catalog;
use pgwire::{
    api::{ClientInfo, Type, stmt::QueryParser},
    error::{ErrorInfo, PgWireError, PgWireResult},
};
use sqlparser::{ast::Statement, dialect::PostgreSqlDialect, parser::Parser};

const DIALECT: PostgreSqlDialect = PostgreSqlDialect {};

#[derive(Clone)]
pub struct NexusQueryParser {
    catalog: Arc<Catalog>,
}

#[derive(Debug, Clone)]
pub enum NexusStatement {
    PeerDDL {
        stmt: Statement,
        ddl: Box<PeerDDL>,
    },
    PeerQuery {
        stmt: Statement,
        assoc: QueryAssociation,
    },
    PeerCursor {
        stmt: Statement,
        cursor: CursorEvent,
    },
    Rollback {
        stmt: Statement,
    },
    Empty,
}

impl NexusStatement {
    pub fn new(
        peers: HashMap<String, pt::peerdb_peers::Peer>,
        stmt: &Statement,
    ) -> PgWireResult<Self> {
        let ddl = PeerDDLAnalyzer.analyze(stmt).map_err(|e| {
            PgWireError::UserError(Box::new(ErrorInfo::new(
                "ERROR".to_owned(),
                "internal_error".to_owned(),
                e.to_string(),
            )))
        })?;

        if let Some(ddl) = ddl {
            return Ok(NexusStatement::PeerDDL {
                stmt: stmt.clone(),
                ddl: Box::new(ddl),
            });
        }

        if let Ok(Some(cursor)) = PeerCursorAnalyzer.analyze(stmt) {
            return Ok(NexusStatement::PeerCursor {
                stmt: stmt.clone(),
                cursor,
            });
        }

        let assoc = {
            let pea = PeerExistanceAnalyzer::new(&peers);
            pea.analyze(stmt).map_err(|e| {
                PgWireError::UserError(Box::new(ErrorInfo::new(
                    "ERROR".to_owned(),
                    "feature_not_supported".to_owned(),
                    e.to_string(),
                )))
            })
        }?;

        Ok(NexusStatement::PeerQuery {
            stmt: stmt.clone(),
            assoc,
        })
    }
}

#[derive(Debug, Clone)]
pub struct NexusParsedStatement {
    pub statement: NexusStatement,
    pub query: String,
}

impl NexusQueryParser {
    pub fn new(catalog: Arc<Catalog>) -> Self {
        Self { catalog }
    }

    pub async fn get_peers_bridge(&self) -> PgWireResult<HashMap<String, pt::peerdb_peers::Peer>> {
        let peers = self.catalog.get_peers().await;

        peers.map_err(|e| {
            PgWireError::UserError(Box::new(ErrorInfo::new(
                "ERROR".to_owned(),
                "internal_error".to_owned(),
                e.to_string(),
            )))
        })
    }

    pub async fn parse_simple_sql(&self, sql: &str) -> PgWireResult<NexusParsedStatement> {
        let mut stmts =
            Parser::parse_sql(&DIALECT, sql).map_err(|e| PgWireError::ApiError(Box::new(e)))?;
        if stmts.len() > 1 {
            let err_msg = format!("unsupported sql: {sql}, statements: {stmts:?}");
            // TODO (kaushik): Better error message for this. When do we start seeing multiple statements?
            Err(PgWireError::UserError(Box::new(ErrorInfo::new(
                "ERROR".to_owned(),
                "42P14".to_owned(),
                err_msg,
            ))))
        } else if stmts.is_empty() {
            Ok(NexusParsedStatement {
                statement: NexusStatement::Empty,
                query: sql.to_owned(),
            })
        } else {
            let stmt = stmts.remove(0);
            if matches!(stmt, Statement::Rollback { .. }) {
                Ok(NexusParsedStatement {
                    statement: NexusStatement::Rollback { stmt },
                    query: sql.to_owned(),
                })
            } else {
                let peers = self.get_peers_bridge().await?;
                let nexus_stmt = NexusStatement::new(peers, &stmt)?;
                Ok(NexusParsedStatement {
                    statement: nexus_stmt,
                    query: sql.to_owned(),
                })
            }
        }
    }
}

#[async_trait]
impl QueryParser for NexusQueryParser {
    type Statement = NexusParsedStatement;

    async fn parse_sql<C>(
        &self,
        _client: &C,
        sql: &str,
        _types: &[Type],
    ) -> PgWireResult<Self::Statement>
    where
        C: ClientInfo + Unpin + Send + Sync,
    {
        let mut stmts =
            Parser::parse_sql(&DIALECT, sql).map_err(|e| PgWireError::ApiError(Box::new(e)))?;
        if stmts.len() > 1 {
            let err_msg = format!("unsupported sql: {sql}, statements: {stmts:?}");
            Err(PgWireError::UserError(Box::new(ErrorInfo::new(
                "ERROR".to_owned(),
                "42P14".to_owned(),
                err_msg,
            ))))
        } else if stmts.is_empty() {
            Ok(NexusParsedStatement {
                statement: NexusStatement::Empty,
                query: sql.to_owned(),
            })
        } else {
            let stmt = stmts.remove(0);
            let peers = self.get_peers_bridge().await?;
            let nexus_stmt = NexusStatement::new(peers, &stmt)?;
            Ok(NexusParsedStatement {
                statement: nexus_stmt,
                query: sql.to_owned(),
            })
        }
    }
}
