#include <gtest/gtest.h>
#include <ctime>
#include <functional>
#include <iomanip>
#include <thread>
#include <utility>

#include "absl/strings/str_format.h"
#include "src/common/base/base.h"
#include "src/stirling/bpftrace_connector.h"
#include "src/stirling/info_class_manager.h"
#include "src/stirling/seq_gen_connector.h"
#include "src/stirling/sequence_generator.h"
#include "src/stirling/source_registry.h"
#include "src/stirling/stirling.h"
#include "src/stirling/types.h"

#include "src/stirling/proto/collector_config.pb.h"

using PubProto = pl::stirling::stirlingpb::Publish;
using SubProto = pl::stirling::stirlingpb::Subscribe;

using pl::stirling::DataElement;
using pl::stirling::SeqGenConnector;
using pl::stirling::SourceRegistry;
using pl::stirling::Stirling;

using pl::types::ColumnWrapperRecordBatch;
using pl::types::DataType;
using pl::types::Float64Value;
using pl::types::Int64Value;
using pl::types::StringValue;
using pl::types::Time64NSValue;

using pl::ConstVectorView;

// Test arguments, from the command line
DEFINE_uint64(kRNGSeed, gflags::Uint64FromEnv("seed", 377), "Random Seed");
DEFINE_uint64(kNumSources, gflags::Uint64FromEnv("num_sources", 2), "Number of sources");
DEFINE_uint64(kNumIterMin, gflags::Uint64FromEnv("num_iter_min", 10), "Min number of iterations");
DEFINE_uint64(kNumIterMax, gflags::Uint64FromEnv("num_iter_max", 20), "Max number of iterations");
DEFINE_uint64(kNumProcessedRequirement, gflags::Uint64FromEnv("num_processed_required", 5000),
              "Number of records required to be processed before test is allowed to end");

// This is the duration for which a subscription will be valid.
constexpr std::chrono::milliseconds kDurationPerIter{500};

// Fraction of times a subscription will subscribe to a source.
const double kSubscribeProb = 0.7;

// If test typically takes too long, you may want to reduce kNumIterMin or kNumProcessedRequirement.
// Note that kNumIterMax * kDurationPerIter defines the maximum time the test can take.

class StirlingTest : public ::testing::Test {
 private:
  std::unique_ptr<Stirling> stirling_;
  PubProto publish_proto_;
  std::unordered_map<uint64_t, std::string> id_to_name_map_;

  // Schemas
  std::unordered_map<uint64_t, const ConstVectorView<DataElement>*> schemas_;

  // Reference model (checkers).
  std::unordered_map<uint64_t, std::unique_ptr<pl::stirling::Sequence<int64_t>>> int_seq_checker_;
  std::unordered_map<uint64_t, std::unique_ptr<pl::stirling::Sequence<double>>> double_seq_checker_;

  std::unordered_map<uint64_t, uint64_t> num_processed_per_table_;
  std::atomic<uint64_t> num_processed_;

  // Random distributions for test parameters.
  std::default_random_engine rng;
  std::uniform_int_distribution<uint32_t> sampling_period_millis_dist_;
  std::uniform_int_distribution<uint32_t> push_period_millis_dist_;
  std::uniform_real_distribution<double> uniform_probability_dist_;

 public:
  inline static const uint64_t& kRNGSeed = FLAGS_kRNGSeed;
  inline static const uint64_t& kNumSources = FLAGS_kNumSources;
  inline static const uint64_t& kNumIterMin = FLAGS_kNumIterMin;
  inline static const uint64_t& kNumIterMax = FLAGS_kNumIterMax;
  inline static const uint64_t& kNumProcessedRequirement = FLAGS_kNumProcessedRequirement;

  StirlingTest()
      : rng(kRNGSeed),
        sampling_period_millis_dist_(0, 10),
        push_period_millis_dist_(0, 100),
        uniform_probability_dist_(0, 1.0) {}

  void SetUp() override {
    // Make registry with a number of SeqGenConnectors.
    std::unique_ptr<SourceRegistry> registry = std::make_unique<SourceRegistry>();
    for (uint32_t i = 0; i < kNumSources; ++i) {
      registry->RegisterOrDie<SeqGenConnector>(absl::StrFormat("sequences%u", i));
    }

    // Make Stirling.
    stirling_ = Stirling::Create(std::move(registry));

    // Set a dummy callback function (normally this would be in the agent).
    stirling_->RegisterCallback(
        std::bind(&StirlingTest::AppendData, this, std::placeholders::_1, std::placeholders::_2));

    stirling_->GetPublishProto(&publish_proto_);

    id_to_name_map_ = stirling_->TableIDToNameMap();

    for (const auto& [id, name] : id_to_name_map_) {
      if (name[name.length() - 1] == '0') {
        schemas_.emplace(id, &SeqGenConnector::kSeq0Table.elements());

        uint32_t col_idx = 1;  // Start at 1, because column 0 is time.
        int_seq_checker_.emplace((id << 32) | col_idx++,
                                 std::make_unique<pl::stirling::LinearSequence<int64_t>>(1, 1));
        int_seq_checker_.emplace((id << 32) | col_idx++,
                                 std::make_unique<pl::stirling::ModuloSequence<int64_t>>(10));
        int_seq_checker_.emplace(
            (id << 32) | col_idx++,
            std::make_unique<pl::stirling::QuadraticSequence<int64_t>>(1, 0, 0));
        int_seq_checker_.emplace((id << 32) | col_idx++,
                                 std::make_unique<pl::stirling::FibonacciSequence<int64_t>>());
        double_seq_checker_.emplace(
            (id << 32) | col_idx++,
            std::make_unique<pl::stirling::LinearSequence<double>>(3.14159, 0));

        num_processed_per_table_.emplace(id, 0);
      } else if (name[name.length() - 1] == '1') {
        schemas_.emplace(id, &SeqGenConnector::kSeq1Table.elements());

        uint32_t col_idx = 1;  // Start at 1, because column 0 is time.
        int_seq_checker_.emplace((id << 32) | col_idx++,
                                 std::make_unique<pl::stirling::LinearSequence<int64_t>>(2, 2));
        int_seq_checker_.emplace((id << 32) | col_idx++,
                                 std::make_unique<pl::stirling::ModuloSequence<int64_t>>(8));

        num_processed_per_table_.emplace(id, 0);
      }
    }

    num_processed_ = 0;
  }

  void TearDown() override {
    for (const auto& [id, name] : id_to_name_map_) {
      LOG(INFO) << absl::StrFormat("Number of records processed: %u", num_processed_per_table_[id]);
      PL_UNUSED(name);
    }
  }

  Stirling* GetStirling() { return stirling_.get(); }

  SubProto GenerateRandomSubscription(const PubProto& publish_proto) {
    SubProto subscribe_proto;

    for (int i = 0; i < publish_proto.published_info_classes_size(); ++i) {
      auto sub_info_class = subscribe_proto.add_subscribed_info_classes();
      sub_info_class->MergeFrom(publish_proto.published_info_classes(i));

      sub_info_class->set_subscribed(uniform_probability_dist_(rng) < kSubscribeProb);
      sub_info_class->set_sampling_period_millis(sampling_period_millis_dist_(rng));
      sub_info_class->set_push_period_millis(push_period_millis_dist_(rng));
    }
    return subscribe_proto;
  }

  SubProto GenerateRandomSubscription() { return GenerateRandomSubscription(publish_proto_); }

  void AppendData(uint64_t table_id, std::unique_ptr<ColumnWrapperRecordBatch> record_batch) {
    // Note: Implicit assumption (not checked here) is that all columns have the same size
    uint64_t num_records = (*record_batch)[0]->Size();
    CheckRecordBatch(table_id, num_records, *record_batch);
  }

  void CheckRecordBatch(const uint32_t table_id, uint64_t num_records,
                        const ColumnWrapperRecordBatch& record_batch) {
    auto table_schema = *(schemas_[table_id]);

    for (uint32_t i = 0; i < num_records; ++i) {
      uint32_t j = 0;

      for (auto col : record_batch) {
        uint64_t key = static_cast<uint64_t>(table_id) << 32 | j;

        switch (table_schema[j].type()) {
          case DataType::TIME64NS: {
            auto val = col->Get<Time64NSValue>(i).val;
            PL_UNUSED(val);
          } break;
          case DataType::INT64: {
            auto& checker = *(int_seq_checker_.at(key));
            EXPECT_EQ(checker(), col->Get<Int64Value>(i).val);
          } break;
          case DataType::FLOAT64: {
            auto& checker = *(double_seq_checker_.at(key));
            EXPECT_EQ(checker(), col->Get<Float64Value>(i).val);
          } break;
          default:
            CHECK(false) << absl::StrFormat("Unrecognized type: $%s",
                                            ToString(table_schema[j].type()));
        }

        j++;
      }
    }

    num_processed_per_table_[table_id] += num_records;
    num_processed_ += num_records;
  }

  uint64_t NumProcessed() { return num_processed_; }
};

// Stress/regression test that hammers Stirling with sequences.
// A reference model checks the sequences are correct on the callback.
// This version uses synchronized subscriptions that occur while Stirling is stopped.
TEST_F(StirlingTest, hammer_time_on_stirling_synchronized_subscriptions) {
  pl::Status s;

  Stirling* stirling = GetStirling();

  uint32_t i = 0;
  while (NumProcessed() < kNumProcessedRequirement || i < kNumIterMin) {
    // Process a subscription message.
    s = stirling->SetSubscription(GenerateRandomSubscription());
    ASSERT_TRUE(s.ok());

    // Run Stirling data collector.
    s = stirling->RunAsThread();
    ASSERT_TRUE(s.ok());

    // Stay in this config for the specified amount of time.
    std::this_thread::sleep_for(kDurationPerIter);

    stirling->Stop();
    stirling->WaitForThreadJoin();

    i++;

    // In case we have a slow environment, break out of the test after some time.
    if (i > kNumIterMax) {
      break;
    }
  }

  EXPECT_GT(NumProcessed(), 0);
}

// Stress/regression test that hammers Stirling with sequences.
// A reference model checks the sequences are correct on the callback.
// This version uses on-the-fly subscriptions that occur while Stirling is running.
TEST_F(StirlingTest, hammer_time_on_stirling_on_the_fly_subs) {
  pl::Status s;

  Stirling* stirling = GetStirling();

  // Run Stirling data collector.
  s = stirling->RunAsThread();
  ASSERT_TRUE(s.ok());

  std::this_thread::sleep_for(kDurationPerIter);

  // Default should be that nothing is subscribed.
  EXPECT_EQ(NumProcessed(), 0);

  uint32_t i = 0;
  while (NumProcessed() < kNumProcessedRequirement || i < kNumIterMin) {
    // Process a subscription message.
    s = stirling->SetSubscription(GenerateRandomSubscription());
    ASSERT_TRUE(s.ok());

    // Stay in this config for the specified amount of time..
    std::this_thread::sleep_for(kDurationPerIter);

    i++;

    // In case we have a slow environment, break out of the test after some time.
    if (i > kNumIterMax) {
      break;
    }
  }

  stirling->Stop();
  stirling->WaitForThreadJoin();

  EXPECT_GT(NumProcessed(), 0);
}

TEST_F(StirlingTest, no_callback_defined) {
  Stirling* stirling = GetStirling();
  stirling->RegisterCallback(nullptr);

  // Should fail to run as a Stirling-managed thread.
  EXPECT_NOT_OK(stirling->RunAsThread());

  // Should also fail to run as a caller-managed thread,
  // which means it should be immediately joinable.
  std::thread run_thread = std::thread(&Stirling::Run, stirling);
  ASSERT_TRUE(run_thread.joinable());
  run_thread.join();
}
