use tonic::Status;

pub fn database_status(err: sqlx::Error) -> Status {
    match err {
        sqlx::Error::Database(ref db) => match db.code().as_deref() {
            Some("23505") => Status::already_exists("username already exists"),
            // PostgreSQL cancels statements that exceed statement_timeout.
            Some("57014") => Status::deadline_exceeded("database operation timed out"),
            _ => internal_database_error(),
        },
        sqlx::Error::PoolTimedOut | sqlx::Error::Io(_) | sqlx::Error::PoolClosed => {
            tracing::warn!(kind = "database_unavailable", "database request failed");
            Status::unavailable("account store unavailable")
        }
        _ => internal_database_error(),
    }
}

fn internal_database_error() -> Status {
    // Database messages can include user input, connection strings or SQL details.
    tracing::error!(kind = "database_error", "database request failed");
    Status::internal("account operation failed")
}

#[cfg(test)]
mod tests {
    use super::*;
    use tonic::Code;

    #[test]
    fn hides_internal_errors() {
        let status = database_status(sqlx::Error::Protocol("secret database details".into()));
        assert_eq!(status.code(), Code::Internal);
        assert!(!status.message().contains("secret"));
    }

    #[test]
    fn pool_timeout_is_unavailable() {
        assert_eq!(
            database_status(sqlx::Error::PoolTimedOut).code(),
            Code::Unavailable
        );
    }

    #[test]
    fn statement_timeout_is_deadline_exceeded() {
        let err = sqlx::Error::Database(Box::new(TestDbError("57014")));
        assert_eq!(database_status(err).code(), Code::DeadlineExceeded);
    }

    #[test]
    fn other_database_errors_stay_internal() {
        let err = sqlx::Error::Database(Box::new(TestDbError("23502")));
        assert_eq!(database_status(err).code(), Code::Internal);
    }

    struct TestDbError(&'static str);

    impl std::fmt::Debug for TestDbError {
        fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
            f.write_str(self.0)
        }
    }

    impl std::fmt::Display for TestDbError {
        fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
            f.write_str(self.0)
        }
    }

    impl std::error::Error for TestDbError {}

    impl sqlx::error::DatabaseError for TestDbError {
        fn message(&self) -> &str {
            self.0
        }
        fn code(&self) -> Option<std::borrow::Cow<'_, str>> {
            Some(self.0.into())
        }
        fn as_error(&self) -> &(dyn std::error::Error + Send + Sync + 'static) {
            self
        }
        fn as_error_mut(&mut self) -> &mut (dyn std::error::Error + Send + Sync + 'static) {
            self
        }
        fn into_error(self: Box<Self>) -> Box<dyn std::error::Error + Send + Sync + 'static> {
            self
        }
        fn kind(&self) -> sqlx::error::ErrorKind {
            sqlx::error::ErrorKind::Other
        }
    }
}
