#ifndef __GF16_CONTROLLER_CPU
#define __GF16_CONTROLLER_CPU

#include "controller.h"
#include <atomic>
#include "threadqueue.h"
#include "gf16mul.h"

class PAR2ProcCPUStaging : public IPAR2ProcStaging {
public:
	void* src;
	std::atomic<int> procRefs;

	PAR2ProcCPUStaging() : IPAR2ProcStaging(), src(nullptr) {}
	~PAR2ProcCPUStaging();
};

class PAR2ProcCPU : public IPAR2ProcBackend {
private:
	size_t sliceSize;
	size_t alignedSliceSize;
	size_t currentSliceSize;
	size_t alignedCurrentSliceSize;

	int numThreads;
	std::vector<MessageThread> thWorkers;
	std::vector<void*> gfScratch;

	Galois16Mul* gf;
	size_t chunkLen;
	size_t numChunks;
	unsigned alignment;
	unsigned stride;
	void freeGf();

	std::vector<PAR2ProcCPUStaging> staging;
	bool reallocMemInput();
	void* memProcessing;

	void calcChunkSize();

	MessageThread transferThread;

	void set_coeffs(PAR2ProcCPUStaging& area, unsigned idx, uint16_t inputNum);
	void set_coeffs(PAR2ProcCPUStaging& area, unsigned idx, const uint16_t* inputCoeffs);
	void run_kernel(unsigned inBuf, unsigned numInputs) override;

	template<typename T> FUTURE_RETURN_T _addInput(const void* buffer, size_t size, T inputNumOrCoeffs, bool flush  IF_LIBUV(, const PAR2ProcPlainCb& cb));

#ifdef USE_LIBUV
	void _notifySent(void* _req) override;
	void _notifyRecv(void* _req) override;
	void _notifyProc(void* _req) override;
#endif

	static void transfer_slice(ThreadMessageQueue<void*>& q);
	static void compute_worker(ThreadMessageQueue<void*>& q);

	PAR2ProcCPU(const PAR2ProcCPU&);
	PAR2ProcCPU& operator=(const PAR2ProcCPU&);

public:
	explicit PAR2ProcCPU(IF_LIBUV(uv_loop_t* _loop,) int stagingAreas=2);
	void setSliceSize(size_t _sliceSize) override;
	void _deinit() override;
	~PAR2ProcCPU();

	bool init(Galois16Methods method = GF16_AUTO, unsigned inputGrouping = 0, size_t chunkLen = 0);
	bool setCurrentSliceSize(size_t newSliceSize) override;

	bool setRecoverySlices(unsigned numSlices, const uint16_t* exponents = NULL) override;
	void freeProcessingMem() override;

	inline void _setAreaActive(int area, bool active) {
		staging[area].setIsActive(active);
	}

	void setNumThreads(int threads);
	inline int getNumThreads() const { return numThreads; }
	inline const char* getMethodName() const { return gf->info().name; }
	inline size_t getChunkLen() const { return chunkLen; }
	inline unsigned getStagingAreas() const { return staging.size(); }
	inline unsigned getAlignment() const { return alignment; }
	inline unsigned getStride() const { return stride; }
	inline size_t getAllocSliceSize() const { return alignedSliceSize; }

	PAR2ProcBackendAddResult canAdd() const override;
	FUTURE_RETURN_T addInput(const void* buffer, size_t size, uint16_t inputNum, bool flush  IF_LIBUV(, const PAR2ProcPlainCb& cb)) override;
	FUTURE_RETURN_T addInput(const void* buffer, size_t size, const uint16_t* coeffs, bool flush  IF_LIBUV(, const PAR2ProcPlainCb& cb)) override;
	void dummyInput(uint16_t inputNum, bool flush = false) override;
	bool fillInput(const void* buffer) override;
	void flush() override;
	FUTURE_RETURN_BOOL_T getOutput(unsigned index, void* output  IF_LIBUV(, const PAR2ProcOutputCb& cb)) override;

	void processing_finished() override;
#ifndef USE_LIBUV
	void waitForAdd() override;
	FUTURE_RETURN_T endInput() override {
		return IPAR2ProcBackend::_endInput(staging);
	}
#endif

	static inline Galois16Methods default_method() { return Galois16Mul::default_method(); }
	static inline Galois16MethodInfo info(Galois16Methods method) { return Galois16Mul::info(method); }
	static inline std::vector<Galois16Methods> availableMethods() { return Galois16Mul::availableMethods(true); }
};

#endif // defined(__GF16_CONTROLLER_CPU)
