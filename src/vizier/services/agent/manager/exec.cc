#include "src/vizier/services/agent/manager/exec.h"

#include <memory>
#include <string>
#include <utility>

#include <jwt/jwt.hpp>

#include "src/common/base/base.h"
#include "src/common/event/task.h"
#include "src/common/perf/perf.h"
#include "src/vizier/services/agent/manager/manager.h"

namespace pl {
namespace vizier {
namespace agent {

using ::pl::event::AsyncTask;

class ExecuteQueryMessageHandler::ExecuteQueryTask : public AsyncTask {
 public:
  ExecuteQueryTask(ExecuteQueryMessageHandler* h, carnot::Carnot* carnot,
                   std::unique_ptr<messages::VizierMessage> msg)
      : parent_(h),
        carnot_(carnot),
        msg_(std::move(msg)),
        req_(msg_->execute_query_request()),
        query_id_(ParseUUID(req_.query_id()).ConsumeValueOrDie()) {}

  sole::uuid query_id() { return query_id_; }

  void Work() override {
    VLOG(1) << absl::Substitute("Executing query: id=$0", query_id_.str());
    VLOG(1) << absl::Substitute("Query Plan: $0=$1", query_id_.str(), req_.plan().DebugString());

    auto s = carnot_->ExecutePlan(req_.plan(), query_id_, req_.analyze());
    if (!s.ok()) {
      LOG(ERROR) << absl::Substitute("Query failed, reason: $0, plan: $1", s.ToString(),
                                     req_.plan().DebugString());
    }
  }

  void Done() override { parent_->HandleQueryExecutionComplete(query_id_); }

 private:
  ExecuteQueryMessageHandler* parent_;
  carnot::Carnot* carnot_;

  std::unique_ptr<messages::VizierMessage> msg_;
  const messages::ExecuteQueryRequest& req_;
  sole::uuid query_id_;
};

ExecuteQueryMessageHandler::ExecuteQueryMessageHandler(pl::event::Dispatcher* dispatcher,
                                                       Info* agent_info,
                                                       Manager::VizierNATSConnector* nats_conn,
                                                       carnot::Carnot* carnot)
    : MessageHandler(dispatcher, agent_info, nats_conn), carnot_(carnot) {}

Status ExecuteQueryMessageHandler::HandleMessage(std::unique_ptr<messages::VizierMessage> msg) {
  // Create a task and run it on the threadpool.
  auto task = std::make_unique<ExecuteQueryTask>(this, carnot_, std::move(msg));

  auto query_id = task->query_id();
  auto runnable = dispatcher()->CreateAsyncTask(std::move(task));
  auto runnable_ptr = runnable.get();
  LOG(INFO) << "Queries in flight: " << running_queries_.size();
  running_queries_[query_id] = std::move(runnable);
  runnable_ptr->Run();

  return Status::OK();
}

void ExecuteQueryMessageHandler::HandleQueryExecutionComplete(sole::uuid query_id) {
  // Upon completion of the query, we makr the runnable task for deletion.
  auto node = running_queries_.extract(query_id);
  if (node.empty()) {
    LOG(ERROR) << "Attempting to delete non-existent query: " << query_id.str();
    return;
  }
  dispatcher()->DeferredDelete(std::move(node.mapped()));
}

}  // namespace agent
}  // namespace vizier
}  // namespace pl
